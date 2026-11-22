package panel

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// HA one-click deployment (PR1): join tokens, HA status APIs and the sealed
// Cloudflare token. The install script (PR2) and the HA page (PR3) build on
// these; nothing here runs system commands or promotes anything.

const (
	// haJoinTokenTTL is how long a join token stays valid.
	haJoinTokenTTL = 10 * time.Minute
	// haWGPort is the WireGuard UDP port both instances use.
	haWGPort = 51820
	// haDefaultPeerWGIP is the WireGuard address assigned to the joining
	// standby when ha.peer_wg_ip is not set.
	haDefaultPeerWGIP = "10.8.0.2"
)

var errHAJoinTokenUsed = errors.New("join token already used")

// haRole reports this instance's HA role from its own env file (never from
// the shared settings: the primary and the standby read the same settings
// table, so a per-instance value cannot live there).
func (s *Server) haRole() string {
	switch s.cfg.HARole {
	case "primary", "secondary":
		return s.cfg.HARole
	default:
		return "none"
	}
}

// haIsPostgres reports whether the panel runs on PostgreSQL. Replication
// queries only exist there; on SQLite they are skipped, never failed.
func (s *Server) haIsPostgres() bool {
	return s.st.DB.Dialector.Name() == "postgres"
}

// haClusterSecret reads a sealed value from cluster_secrets. A missing row
// or a value that cannot be opened returns "" instead of an error: the
// join config reports empty fields rather than failing.
func (s *Server) haClusterSecret(name string) string {
	var row store.ClusterSecret
	if err := s.st.DB.First(&row, "name = ?", name).Error; err != nil {
		return ""
	}
	return s.st.Box.MustOpen(row.Value)
}

// setHAClusterSecretDB seals value and stores it under name in
// cluster_secrets, overwriting any previous value. It is the shared
// implementation behind the maintenance CLI (ha-seal-secret, used by
// ha-init) and the HTTP endpoints.
func setHAClusterSecretDB(db *gorm.DB, box *secret.BoxSet, name, value string) error {
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "name"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&store.ClusterSecret{Name: name, Value: box.Seal(value)}).Error
}

// setHAClusterSecret seals value and stores it under name, overwriting any
// previous value.
func (s *Server) setHAClusterSecret(name, value string) error {
	return setHAClusterSecretDB(s.st.DB, s.st.Box, name, value)
}

// haDBSize returns the current database size in bytes on PostgreSQL, or nil
// on SQLite / when the query fails. ha-join uses it (via the join-config
// bundle, since /ha/status needs an admin session) to check the standby has
// enough disk for the replica.
func (s *Server) haDBSize() *int64 {
	if !s.haIsPostgres() {
		return nil
	}
	var n int64
	if err := s.st.DB.Raw("SELECT pg_database_size(current_database())").Scan(&n).Error; err != nil {
		return nil
	}
	return &n
}

// haReplica is one streaming replication connection on the primary.
type haReplica struct {
	ClientAddr string `json:"clientAddr"`
	State      string `json:"state"`
	SyncState  string `json:"syncState"`
	AppName    string `json:"appName"`
	LagBytes   *int64 `json:"lagBytes"`
}

// haReplication lists the primary's replication connections. It returns nil
// on SQLite or when the query fails: replication state must never fail the
// whole status call.
func (s *Server) haReplication() []haReplica {
	if !s.haIsPostgres() {
		return nil
	}
	var rows []haReplica
	if err := s.st.DB.Raw(`SELECT client_addr::text AS client_addr, state, sync_state,
		COALESCE(application_name, '') AS app_name,
		pg_wal_lsn_diff(sent_lsn, replay_lsn)::bigint AS lag_bytes FROM pg_stat_replication`).Scan(&rows).Error; err != nil {
		return nil
	}
	if rows == nil {
		return []haReplica{}
	}
	return rows
}

// haSyncReplicationEnabled reports the primary's synchronous_standby_names
// and whether this transaction is actually waiting for a standby ack.
//
// Setting the GUC only makes the primary *wait* when the name it lists
// matches a connected standby's application_name; a mismatched name looks
// configured but still runs fully asynchronous. Reporting both values lets
// the page say which of the two states the cluster is in instead of
// claiming sync replication when nothing is waiting.
func (s *Server) haSyncReplicationEnabled() (names string, waiting bool) {
	if !s.haIsPostgres() {
		return "", false
	}
	if err := s.st.DB.Raw("SELECT COALESCE(current_setting('synchronous_standby_names', true), '')").Scan(&names).Error; err != nil {
		return "", false
	}
	if strings.TrimSpace(names) == "" {
		return "", false
	}
	for _, r := range s.haReplication() {
		if r.SyncState == "sync" || r.SyncState == "quorum" {
			return names, true
		}
	}
	return names, false
}

// haReplayState reports whether this database is in recovery (a standby)
// and how far behind replay is, in seconds. Both are nil when the query
// cannot run (SQLite) or fails.
func (s *Server) haReplayState() (inRecovery *bool, lagSeconds *float64) {
	if !s.haIsPostgres() {
		return nil, nil
	}
	var rec bool
	if err := s.st.DB.Raw("SELECT pg_is_in_recovery()").Scan(&rec).Error; err != nil {
		return nil, nil
	}
	inRecovery = &rec
	var lag *float64
	if err := s.st.DB.Raw("SELECT EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp()))").Scan(&lag).Error; err != nil {
		return inRecovery, nil
	}
	return inRecovery, lag
}

// haStatus is GET /ha/status.
func (s *Server) haStatus(c *gin.Context) {
	members := s.clusterMembers()
	sort.Slice(members, func(i, j int) bool { return members[i].SeenAt.After(members[j].SeenAt) })
	leader := ""
	if s.haIsPostgres() {
		var l store.ClusterLease
		if err := s.st.DB.Where("name = ? AND expires_at > now()", leaseName).First(&l).Error; err == nil {
			leader = l.Holder
		}
	} else if s.isLeader() {
		leader = s.procID
	}
	inRecovery, replayLag := s.haReplayState()
	syncNames, syncWaiting := s.haSyncReplicationEnabled()
	out := gin.H{
		"role":             s.haRole(),
		"members":          members,
		"leader":           leader,
		"shared":           s.bus != nil,
		"replication":      s.haReplication(),
		"inRecovery":       inRecovery,
		"replayLagSeconds": replayLag,
		"syncNames":        syncNames,
		"syncWaiting":      syncWaiting,
		"redisConfigured":  s.cfg.RedisURL != "",
		"dbSizeBytes":      s.haDBSize(),
		"version":          Version,
		"edition":          Edition(),
	}
	// The latest sync upgrade and its per-instance deliveries, so the
	// HA page can show the staged progress without another endpoint.
	if op := s.latestSyncUpdate(); op != nil {
		out["syncUpdate"] = gin.H{"operation": op, "deliveries": s.syncDeliveries(op.ID)}
	}
	c.JSON(http.StatusOK, out)
}

// haIssueToken is POST /ha/token (sudo). It voids unused tokens and mints a
// fresh one, returning the token and the one-click join command.
func (s *Server) haIssueToken(c *gin.Context) {
	tok, err := mintHAJoinToken(s.st.DB, c.GetString("admin"))
	if err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "ha.token", "join", "")
	cmd := "bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh) ha-join " + tok + " " + s.requestBase(c.Request)
	c.JSON(http.StatusOK, gin.H{"token": tok, "command": cmd, "expiresIn": int(haJoinTokenTTL.Seconds())})
}

// haSaveCFToken is POST /ha/cf-token (sudo). The token is sealed into
// cluster_secrets and never echoed back.
func (s *Server) haSaveCFToken(c *gin.Context) {
	var req struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		badRequest(c, errors.New("token is required"))
		return
	}
	if err := s.setHAClusterSecret("cf_api_token", strings.TrimSpace(req.Token)); err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "ha.cf_token", "cloudflare", "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// haSaveSyncReplication is POST /ha/sync-replication (sudo). It turns
// synchronous replication on or off on the primary by setting
// synchronous_standby_names, the option the HA design left to the wizard
// (§12 Q3).
//
// The value comes from ha_peers, never from the request: a name that does
// not match a connected standby's application_name makes PostgreSQL wait
// for a confirmation that never arrives, which would block every write
// instead of protecting it. An empty peer list therefore refuses to enable
// rather than setting a wildcard.
//
// The change is ALTER SYSTEM + pg_reload_conf(), so it survives a restart
// and takes effect without dropping the connection that writes it.
func (s *Server) haSaveSyncReplication(c *gin.Context) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if !s.haIsPostgres() {
		badRequest(c, errors.New("synchronous replication requires PostgreSQL"))
		return
	}
	if s.haRole() != "primary" {
		badRequest(c, errors.New("only the primary configures synchronous replication"))
		return
	}
	value := ""
	if req.Enabled {
		var peers []store.HAPeer
		s.st.DB.Order("created_at").Find(&peers)
		names := make([]string, 0, len(peers))
		for _, p := range peers {
			if strings.TrimSpace(p.Pubkey) != "" {
				names = append(names, haSyncAppName)
			}
		}
		if len(names) == 0 {
			badRequest(c, errors.New("no standby has joined yet: joining the standby first lets PostgreSQL name it"))
			return
		}
		// ONE standby, not ANY n: with a single standby ANY 1 behaves the
		// same, but FIRST 1 cannot fall back to a standby that is not
		// streaming, which is what makes a synchronous commit actually wait.
		value = "FIRST 1 (" + strings.Join(uniqueStrings(names), ",") + ")"
	}
	// Both statements go through one connection in this order: ALTER SYSTEM
	// writes postgresql.auto.conf, the reload applies it. The reload is a
	// separate request because an ALTER SYSTEM inside an explicit
	// transaction is rejected.
	if err := s.st.DB.Exec("ALTER SYSTEM SET synchronous_standby_names = ?", value).Error; err != nil {
		serverError(c, err)
		return
	}
	if err := s.st.DB.Exec("SELECT pg_reload_conf()").Error; err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "ha.sync_replication", value, "")
	names, waiting := s.haSyncReplicationEnabled()
	c.JSON(http.StatusOK, gin.H{"syncNames": names, "syncWaiting": waiting})
}

// haTakeLeader is POST /ha/take-leader (sudo). It asks the current leader
// to step aside so this instance takes over the lease: the manual fix for
// a lease that landed on the wrong instance, since a standby that grabbed
// it while the primary restarted never hands it back on its own. Only the
// request is recorded here (see requestLeaderTakeover); the handover
// completes over the next lease rounds and the page watches /ha/status
// for the leader badge to move.
func (s *Server) haTakeLeader(c *gin.Context) {
	if !s.haIsPostgres() {
		badRequest(c, errors.New("接管 Leader 仅在集群模式（PostgreSQL）下可用，单机面板本身就是 Leader"))
		return
	}
	if s.isLeader() {
		badRequest(c, errors.New("本机已是 Leader"))
		return
	}
	if s.activeSyncUpdate() != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "同步升级进行中，等它结束后再接管 Leader"})
		return
	}
	if err := s.requestLeaderTakeover(); err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "ha.take_leader", "", "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// haSyncAppName is the application_name the installer gives the standby's
// walreceiver, so synchronous_standby_names can name it.
const haSyncAppName = "polaris_standby"

// uniqueStrings drops duplicates while keeping the order.
func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// haListPeers is GET /ha/peers.
func (s *Server) haListPeers(c *gin.Context) {
	var peers []store.HAPeer
	s.st.DB.Order("created_at").Find(&peers)
	if peers == nil {
		peers = []store.HAPeer{}
	}
	c.JSON(http.StatusOK, gin.H{"peers": peers})
}

// haDeletePeer is DELETE /ha/peers/:wg_ip (sudo). It only removes the
// database record; the WireGuard config is reconciled by ha sync-wg.
func (s *Server) haDeletePeer(c *gin.Context) {
	wgIP := c.Param("wg_ip")
	if err := s.st.DB.Delete(&store.HAPeer{}, "wg_ip = ?", wgIP).Error; err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "ha.peer_remove", wgIP, "")
	c.Status(http.StatusNoContent)
}

// haFailoverSteps are the manual promotion steps the HA page shows. They
// are text only: nothing here executes them.
var haFailoverSteps = []string{
	"确认主节点真的宕机（ping / SSH 都不通），而不是面板假死",
	"在备机上提主 PostgreSQL：su postgres -c \"psql -c 'SELECT pg_promote();'\"",
	"在备机上提主 Redis：redis-cli -h 10.8.0.2 -a <密码> REPLICAOF NO ONE",
	"改备机 /etc/polaris/polaris.env，把 POLARIS_DB_DSN 与 POLARIS_REDIS_URL 的 10.8.0.1 改成 10.8.0.2，重启面板",
	"确认 DNS 已切到备机：dig +short polaris.cuijianzhuang.com",
	"验证业务：面板能登录、节点陆续在线、新订阅能拉取",
}

// haFailoverCheck is POST /ha/failover-check (admin, read-only).
func (s *Server) haFailoverCheck(c *gin.Context) {
	inRecovery, replayLag := s.haReplayState()
	host := ""
	if u, err := url.Parse(s.cfg.PublicURL); err == nil {
		host = u.Hostname()
	}
	var dnsAddrs []string
	if host != "" {
		if addrs, err := net.LookupHost(host); err == nil {
			dnsAddrs = addrs
		}
	}
	// Probe the public entrypoint the way a client or agent would reach it.
	// From the standby this reports whether the primary is really serving;
	// from the primary it reports whether its own public address answers.
	// A failed probe is a signal, not an error: it must never fail the check.
	var primaryReachable *bool
	var primaryStatus *int
	if host != "" {
		probeURL := strings.TrimRight(s.cfg.PublicURL, "/") + s.cfg.APIBase() + "/info"
		cli := &http.Client{Timeout: 5 * time.Second}
		if resp, err := cli.Get(probeURL); err == nil {
			ok := resp.StatusCode == http.StatusOK
			primaryReachable = &ok
			code := resp.StatusCode
			primaryStatus = &code
			resp.Body.Close()
		} else {
			no := false
			primaryReachable = &no
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"role":             s.haRole(),
		"healthy":          true,
		"inRecovery":       inRecovery,
		"replayLagSeconds": replayLag,
		"dnsHost":          host,
		"dnsAddresses":     dnsAddrs,
		"primaryReachable": primaryReachable,
		"primaryStatus":    primaryStatus,
		"steps":            haFailoverSteps,
	})
}

// haJoinAuth validates a one-time HA join token from the Authorization
// header and stores the row in the context. It never reveals whether the
// token is unknown, expired or already used.
func (s *Server) haJoinAuth(c *gin.Context) {
	// Guess attempts are cheap, so rate-limit before the token lookup.
	if !s.haJoinLimit.Allow(clientIP(c.Request)) {
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "操作太频繁，请稍后再试"})
		return
	}
	tok := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	fail := func() {
		s.audit("system", "ha.join_denied", "", "", clientIP(c.Request))
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
	}
	if tok == "" {
		fail()
		return
	}
	var t store.HAJoinToken
	if err := s.st.DB.First(&t, "token_hash = ?", hashToken(tok)).Error; err != nil {
		fail()
		return
	}
	if t.UsedAt != nil || time.Now().After(t.ExpiresAt) {
		fail()
		return
	}
	c.Set("haJoinToken", t)
	c.Next()
}

// haJoinConfig is GET /ha/join-config. The token may fetch it repeatedly
// until a peer registers with it.
func (s *Server) haJoinConfig(c *gin.Context) {
	s.audit("system", "ha.join_config", "", "", clientIP(c.Request))
	c.JSON(http.StatusOK, s.haJoinConfigBundle(c.Request))
}

// haJoinConfigBundle assembles the join config. Sealed values that are
// missing or cannot be opened come back empty; the bundle never fails
// because of them. The secret key is only ever served here.
func (s *Server) haJoinConfigBundle(r *http.Request) gin.H {
	domain := ""
	if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Hostname() != "" {
		domain = u.Hostname()
	} else if r != nil {
		if host, _, err := net.SplitHostPort(r.Host); err == nil {
			domain = host
		} else {
			domain = r.Host
		}
	}
	pgVersion := ""
	if s.haIsPostgres() {
		var v string
		if err := s.st.DB.Raw("SHOW server_version").Scan(&v).Error; err == nil {
			pgVersion = haMajorVersion(v)
		}
	}
	// The standby inherits the primary's collation through pg_basebackup,
	// and PostgreSQL refuses to start a cluster whose LC_COLLATE the
	// operating system does not know ("database locale is incompatible
	// with operating system"). The standby is usually a different machine,
	// so send the collation with the bundle and let ha-join check it before
	// copying 80 GB of WAL to find out. Read it from the catalog, not
	// SHOW lc_collate: the SHOW form comes back empty through this
	// driver's Scan (go-postgres red on TestHAJoinTokenFlow at #410),
	// while the catalog value is always populated for a real database.
	pgCollate := ""
	if s.haIsPostgres() {
		var c string
		if err := s.st.DB.Raw("SELECT datcollate FROM pg_database WHERE datname = current_database()").Scan(&c).Error; err == nil {
			pgCollate = c
		}
	}
	// One collation is not enough: pg_basebackup copies the whole cluster,
	// not just the polaris database, so template1 (and any other database)
	// can carry a different LC_COLLATE. The field case was a primary whose
	// polaris database was C while template1 was en_US.UTF-8: checking only
	// the polaris value let the join through and the standby then refused
	// to start. Send every distinct datcollate in the cluster; pg_collate
	// stays as the polaris database's own value for older installers that
	// only read the single field.
	var pgCollates []string
	if s.haIsPostgres() {
		var cs []string
		if err := s.st.DB.Raw("SELECT DISTINCT datcollate FROM pg_database WHERE datcollate <> '' ORDER BY 1").Scan(&cs).Error; err == nil {
			pgCollates = cs
		}
	}
	// The primary's WireGuard identity comes from its own env file: the
	// standby reads the same shared settings, so per-instance values cannot
	// live there. The standby's address defaults to 10.8.0.2 when the peer
	// did not send one.
	peerWGIP := s.cfg.HAPeerWGIP
	if peerWGIP == "" {
		peerWGIP = haDefaultPeerWGIP
	}
	return gin.H{
		"panel_version":       Version,
		"edition":             Edition(),
		"public_url":          s.cfg.PublicURL,
		"admin_path":          s.cfg.AdminPath,
		"timezone":            s.cfg.Timezone,
		"secret_key":          s.cfg.SecretKey,
		"secret_key_previous": s.cfg.SecretKeyPrevious,
		"db_dsn":              s.cfg.DB.DSN,
		"redis_url":           s.cfg.RedisURL,
		"pg_version":          pgVersion,
		"pg_collate":          pgCollate,
		"pg_collates":         pgCollates,
		"repl_password":       s.haClusterSecret("ha_repl_password"),
		"wg": gin.H{
			"pubkey":   s.cfg.HAWGPubkey,
			"endpoint": s.cfg.HAEndpoint,
			"ip":       s.cfg.HAWGIP,
			"port":     haWGPort,
		},
		"wg_ip":         peerWGIP,
		"db_size_bytes": s.haDBSize(),
		"cf_api_token":  s.haClusterSecret("cf_api_token"),
		"domain":        domain,
	}
}

// haMajorVersion reduces "15.4 (Debian 15.4-2)" to "15".
func haMajorVersion(v string) string {
	v = strings.TrimSpace(v)
	for i, r := range v {
		if r == '.' || r == ' ' || r == '-' {
			return v[:i]
		}
	}
	return v
}

// haRegisterPeer is POST /ha/peers. It creates or refreshes the peer row
// and consumes the token in the same transaction, so two concurrent
// registrations cannot both win.
func (s *Server) haRegisterPeer(c *gin.Context) {
	var req struct {
		WGIP     string `json:"wg_ip"`
		Pubkey   string `json:"pubkey"`
		Endpoint string `json:"endpoint"`
		Role     string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	req.WGIP = strings.TrimSpace(req.WGIP)
	req.Pubkey = strings.TrimSpace(req.Pubkey)
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	req.Role = strings.TrimSpace(req.Role)
	if net.ParseIP(req.WGIP) == nil || req.Pubkey == "" || len(req.Pubkey) > 64 {
		badRequest(c, errors.New("wg_ip must be an IP and pubkey is required"))
		return
	}
	if req.Role == "" {
		req.Role = "secondary"
	}
	if req.Role != "primary" && req.Role != "secondary" {
		badRequest(c, errors.New("role must be primary or secondary"))
		return
	}
	t, _ := c.Get("haJoinToken")
	tok := t.(store.HAJoinToken)
	now := time.Now().UTC()
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "wg_ip"}},
			DoUpdates: clause.AssignmentColumns([]string{"pubkey", "endpoint", "role", "last_seen"}),
		}).Create(&store.HAPeer{WGIP: req.WGIP, Pubkey: req.Pubkey, Endpoint: req.Endpoint,
			Role: req.Role, CreatedAt: now, LastSeen: &now}).Error; err != nil {
			return err
		}
		res := tx.Model(&store.HAJoinToken{}).Where("id = ? AND used_at IS NULL", tok.ID).
			Updates(map[string]any{"used_at": now, "used_by_wg_ip": req.WGIP})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errHAJoinTokenUsed
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errHAJoinTokenUsed) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		serverError(c, err)
		return
	}
	s.audit("system", "ha.peer_register", req.WGIP, "", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"ok": true, "wg_ip": req.WGIP})
}
