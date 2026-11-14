// Package panel is the Polaris control plane: REST API, desired-state
// publication, the agent gRPC service, traffic settlement and scheduled jobs.
package panel

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
	"golang.org/x/net/http2"
	"gorm.io/gorm/clause"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/api/gen/polaris/v1/polarisv1connect"
	"github.com/cuijianzhuang/polaris/internal/asn"
	"github.com/cuijianzhuang/polaris/internal/geoip"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"

	// Register every core adapter so capabilities are available.
	_ "github.com/cuijianzhuang/polaris/internal/core/caddy"
	_ "github.com/cuijianzhuang/polaris/internal/core/cloudflared"
	_ "github.com/cuijianzhuang/polaris/internal/core/forwarder"
	_ "github.com/cuijianzhuang/polaris/internal/core/gost"
	_ "github.com/cuijianzhuang/polaris/internal/core/hysteria"
	_ "github.com/cuijianzhuang/polaris/internal/core/mihomo"
	_ "github.com/cuijianzhuang/polaris/internal/core/singbox"
	_ "github.com/cuijianzhuang/polaris/internal/core/xray"
)

// Version is set at build time.
var Version = "dev"

// Server is the panel.
type Server struct {
	renderCache         renderCache // plugin subscription output (plugin_sub.go)
	jobs                jobRegistry // scheduled job state on this instance (jobs.go)
	businessEvents      chan struct{}
	userNoticeBusy      atomic.Bool
	cfg                 *Config
	st                  *store.Store
	ca                  *pki.CA
	log                 *slog.Logger
	hub                 *Hub
	traffic             *trafficInbox
	eph                 *ephStore   // short-lived values shared by every instance (sign-in codes, OAuth states)
	tgWeb               tgWebLogins // login-page Telegram sign-in requests
	portalWeb           tgWebLogins // user center Telegram sign-in requests
	portalBindWeb       tgWebLogins // authenticated user-center account binding
	aiSlots             chan struct{}
	hbMu                sync.Mutex
	probeBusy           atomic.Bool // a probe round is running
	cdnAddressCheckBusy atomic.Bool
	statusCache         atomic.Pointer[statusSnap]  // public status page data
	statusBuildMu       sync.Mutex                  // coalesce concurrent status rebuilds
	statsOvCache        atomic.Pointer[statsOvSnap] // dashboard aggregates (api_stats.go)
	statsOvMu           sync.Mutex                  // coalesce concurrent stats rebuilds
	usageMu             sync.Mutex                  // guards the global usage-count cache
	usageCached         usageCounts                 // license server /usage.json (usage_stats.go)
	usageCachedAt       time.Time
	restartHook         func()                              // tests: replaces the re-exec after a restore
	hbCores             map[string]hbCoreState              // last core state written per node
	progress            map[string]*nodeProgress            // live agent activity per node (guarded by hbMu)
	limited             map[string][]*polarisv1.LimitedUser // per-user speed limits in effect per node (hbMu)
	lic                 atomic.Pointer[licenseState]        // license in effect (see license.go)
	licKeys             []ed25519.PublicKey                 // overrides the built-in license keys (tests)
	jwtKey              []byte                              // "jwt:"+SecretKey: signs new tokens
	jwtPrevKey          []byte                              // "jwt:"+SecretKeyPrevious, during a rotation; nil otherwise
	jwtKid              string                              // kid of SecretKey, stamped on new tokens
	jwtPrevKid          string                              // kid of SecretKeyPrevious, or ""
	rt                  *realtime

	licenseMu              sync.Mutex           // serializes checks, installation and deactivation
	licenseManualAvailable atomic.Int64         // nanosecond timestamp for the UI cooldown
	licenseSchedule        licenseCheckSchedule // guarded by licenseMu

	dirtyMu    sync.Mutex
	dirty      map[string]string
	kick       chan struct{}
	pubMu      sync.Mutex
	relayMu    sync.Mutex // serializes managed relay planning, application and cleanup
	outboundMu sync.Mutex // serializes outbound dependency validation and mutations
	shopMu     sync.Mutex // serializes pending checkout reuse and USDT amount allocation

	// shared holds online IPs, rate limits and alert de-duplication:
	// in memory, or in Redis when redis_url is set (several instances).
	shared sharedState

	routeDatabaseTransport http.RoundTripper // tests: repository download transport

	memberID          string // stable local installation identity across restarts
	opsMu             sync.Mutex
	procID            string // this process, for the leader lease
	restored          bool   // a staged backup was applied at this start
	startedAt         time.Time
	memberAt          time.Time // last cluster_members refresh
	routeGeoMu        sync.Mutex
	routeGeoCache     *routeDatabaseEntry[*geoip.Database]
	routeASNMu        sync.Mutex
	routeASNCache     *routeDatabaseEntry[*asn.Database]
	routeScheduleBusy atomic.Bool
	leader            atomic.Bool // holds the lease: runs scheduled jobs and the Telegram bot
	bus               *clusterBus // messages between instances (PostgreSQL only)
	presMu            sync.Mutex
	presAt            map[string]time.Time // last presence refresh per node
	specAt            map[string]time.Time // last SpecChanged re-sent on a lagging heartbeat
	shareAt           map[string]time.Time // last heartbeat state shared per node
	rtShare           atomic.Bool          // node list changed here: tell the other instances

	subLimit      *sharedLimiter
	shopLimit     *sharedLimiter
	mailCodeLimit *sharedLimiter
	ticketLimit   *sharedLimiter
	callbacks     callbackStore
	nodeUserSets  nodeUsersCache
	schemes       schemeCache
	ghAuth        githubAuth
	webFS         http.FileSystem
	bg            context.Context
	sinks         []chan Event
	health        healthState
	tg            *tgBot
	tgEvents      chan Event
	hookEvents    chan Event
	mailEvents    chan Event
	seenBase      atomic.Value // string: public base URL the admin UI is reached at
	auditChanges  atomic.Bool  // record row changes made by admins (audit_changes)
	changes       changeLog
}

// New opens the database, loads or creates the CA and seeds defaults.
func New(cfg *Config, log *slog.Logger, web http.FileSystem) (*Server, error) {
	cfg.applyJobDefaults() // a Config built in code has zero job intervals; see config.go
	if cfg.AdminPath == "/" {
		log.Warn("admin UI is served at / — set POLARIS_ADMIN_PATH to a hard-to-guess path so scanners only see 404 or the decoy site")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	box, err := secret.NewBoxSet(cfg.SecretKey, cfg.SecretKeyPrevious)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DB.DSN, box, cfg.DB.Debug)
	if err != nil {
		return nil, err
	}
	restored := applyStagedRestore(st, cfg, box, log) // a backup chosen in the UI, before anything reads the data
	// Refuse to start when the database names a key version this panel
	// does not know: a half-finished rotation from a newer key would
	// otherwise silently lose data. The setting is written by the
	// rotation migration (and by a restore); an absent setting means a
	// pre-rotation database, which simply adopts this key.
	if err := validateKeyVersion(st, box); err != nil {
		return nil, err
	}
	ca, err := loadOrCreateCA(st, cfg.DataDir, box)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, st: st, ca: ca, log: log, hub: newHub(),
		jwtKey: []byte("jwt:" + cfg.SecretKey), jwtKid: box.CurrentKid(),
		jwtPrevKid: box.PreviousKid(), rt: newRealtime(),
		dirty: map[string]string{}, kick: make(chan struct{}, 1),
		webFS: web, bg: context.Background(),
	}
	if cfg.SecretKeyPrevious != "" {
		s.jwtPrevKey = []byte("jwt:" + cfg.SecretKeyPrevious)
	}
	s.rt.snap = func() any { return s.nodeViews() }
	s.tg = newTGBot(s)
	s.auditChanges.Store(st.GetSetting("audit_changes", "false") == "true")
	s.registerChangeLog()
	tgEvents := make(chan Event, 256)
	s.sinks = append(s.sinks, tgEvents)
	s.tgEvents = tgEvents
	s.businessEvents = make(chan struct{}, 1)
	s.hookEvents = make(chan Event, 1024)
	s.sinks = append(s.sinks, s.hookEvents)
	s.mailEvents = make(chan Event, 256)
	s.sinks = append(s.sinks, s.mailEvents)
	s.procID = store.NewID()
	s.memberID, err = loadMemberID(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s.restored, s.startedAt = restored, time.Now()
	s.eph = &ephStore{db: st.DB, box: box}
	s.ghAuth = githubAuth{eph: s.eph}
	s.callbacks = callbackStore{eph: s.eph}
	s.tgWeb = tgWebLogins{eph: s.eph, name: "tgweb"}
	s.portalWeb = tgWebLogins{eph: s.eph, name: "portalweb"}
	s.portalBindWeb = tgWebLogins{eph: s.eph, name: "portalbind"}
	s.aiSlots = make(chan struct{}, 4)
	s.leader.Store(s.tryLease()) // SQLite: always; PostgreSQL: if no live leader holds the lease
	s.presAt, s.specAt, s.shareAt = map[string]time.Time{}, map[string]time.Time{}, map[string]time.Time{}
	if st.DB.Dialector.Name() == "postgres" {
		// Other instances may share this database: reach their agents.
		s.bus = newClusterBus(cfg.DB.DSN, st.DB, s.procID, log)
		s.hub.remote = newClusterHub(s, s.bus)
		s.bus.subscribe("setting", func(from string, data []byte) {
			var key string
			if from != s.procID && json.Unmarshal(data, &key) == nil {
				st.ForgetSetting(key)
			}
		})
		s.bus.subscribe("nodes", func(from string, _ []byte) {
			if from != s.procID {
				s.rt.dirty.Store(true)
			}
		})
		st.OnSettingChange = func(key string) { _ = s.bus.publish("setting", key) }
		s.subscribeClusterOps()
		s.bus.subscribe("cluster-ops", func(_ string, _ []byte) { s.processClusterOperations() })
	}
	s.traffic = &trafficInbox{db: st.DB}
	s.shared = newMemState()
	if cfg.RedisURL != "" {
		rs, err := newRedisState(cfg.RedisURL, log)
		if err != nil {
			return nil, err
		}
		s.shared = rs
	}
	s.subLimit = &sharedLimiter{s: s.shared, name: "sub", perMinute: cfg.SubRateLimit}
	s.shopLimit = &sharedLimiter{s: s.shared, name: "shop", perMinute: 10}
	s.mailCodeLimit = &sharedLimiter{s: s.shared, name: "mailcode", perMinute: 5}
	s.ticketLimit = &sharedLimiter{s: s.shared, name: "ticket", perMinute: 6}
	if err := s.seed(); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	s.initLicense()
	s.memberTouch()
	return s, nil
}

func (s *Server) ctxBG() context.Context { return s.bg }

// Store exposes the repository (used by tests and the CLI).
func (s *Server) Store() *store.Store { return s.st }

// loadOrCreateCA returns the internal CA. It lives in the database so every
// panel instance on it shares one CA; older installs had it only in the
// data directory, which is imported once. The files are kept in step (the
// backup format carries them).
func loadOrCreateCA(st *store.Store, dir string, box *secret.BoxSet) (*pki.CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	fromDB := func() (cert, key string) {
		var rows []store.ClusterSecret
		st.DB.Where("name IN ?", []string{"ca.crt", "ca.key"}).Find(&rows)
		for _, r := range rows {
			if r.Name == "ca.crt" {
				cert = r.Value
			} else {
				key = r.Value
			}
		}
		return
	}
	cert, key := fromDB()
	if cert == "" || key == "" {
		certPEM, err1 := os.ReadFile(certPath)
		keySealed, err2 := os.ReadFile(keyPath)
		if err1 != nil || err2 != nil {
			ca, err := pki.NewCA("Polaris Internal CA")
			if err != nil {
				return nil, err
			}
			certPEM, keySealed = ca.CertPEM, []byte(box.Seal(string(ca.KeyPEM)))
		}
		// Another instance starting at the same moment may win; its CA is
		// read back below.
		for _, r := range []store.ClusterSecret{{Name: "ca.crt", Value: string(certPEM)}, {Name: "ca.key", Value: string(keySealed)}} {
			if err := st.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&r).Error; err != nil {
				return nil, fmt.Errorf("store CA: %w", err)
			}
		}
		cert, key = fromDB()
	}
	keyPEM, err := box.Open(key)
	if err != nil {
		return nil, fmt.Errorf("decrypt CA key (wrong secret_key?): %w", err)
	}
	ca, err := pki.LoadCA([]byte(cert), []byte(keyPEM))
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(certPath); err != nil || string(b) != cert {
		_ = os.WriteFile(certPath, []byte(cert), 0o644)
		_ = os.WriteFile(keyPath, []byte(key), 0o600)
	}
	return ca, nil
}

// Run serves HTTP and the agent endpoint until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.bg = ctx
	s.updateLease() // decided before the jobs start
	if s.isLeader() {
		s.onLeader()
	}
	s.memberTouch()
	go s.runLease(ctx)
	go s.runClusterOperations(ctx)
	if s.bus != nil {
		go s.bus.run(ctx)
		go s.shareNodeChanges(ctx)
		if s.restored {
			// The others still hold the old CA, settings and caches.
			// The restore transaction already persisted the operation. These
			// notifications only wake new peers and reach legacy peers.
			_ = s.bus.publish("cluster-ops", "")
			if err := s.bus.publish(topicRestoreDone, s.procID); err != nil {
				s.log.Warn("tell other instances about the restore", "err", err)
			}
		}
	}
	go s.runReconciler(ctx)
	go s.runAgentSync(ctx)
	go s.rt.run(ctx)
	go s.tg.run(ctx)
	go s.tg.runNotifications(ctx, s.tgEvents)
	go s.runWebhooks(ctx, s.hookEvents)
	go s.runMail(ctx, s.mailEvents)
	licenseCtx, stopLicense := context.WithCancel(ctx)
	defer stopLicense()
	go s.runLicenseChecks(licenseCtx)
	stopJobs := s.startJobs()
	defer stopJobs()
	// A rotation in progress resumes here; the tick is a no-op once the
	// migration is done and no previous key is configured.
	go s.secretMigrationTick()

	// No ReadTimeout: it would also cut long uploads and the hijacked
	// /ws connection. Bodies are capped by bodyLimit instead; idle
	// keep-alive connections are closed after two minutes.
	httpSrv := &http.Server{Addr: s.cfg.Listen, Handler: s.Router(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}

	agentSrv, err := s.agentServer()
	if err != nil {
		return err
	}
	errc := make(chan error, 2)
	go func() {
		s.log.Info("panel listening", "addr", s.cfg.Listen, "admin", s.cfg.AdminPath)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go func() {
		s.log.Info("agent endpoint listening", "addr", s.cfg.AgentListen, "ca_sha256", s.ca.Fingerprint())
		if err := agentSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(sctx)
	_ = agentSrv.Shutdown(sctx)
	return s.settleTraffic()
}

// agentServer builds the mTLS endpoint. Client certificates are verified when
// presented; Enroll is the only RPC that works without one.
func (s *Server) agentServer() (*http.Server, error) {
	hosts := append([]string{"localhost", "127.0.0.1"}, s.cfg.AgentHosts...)
	cert, err := s.ca.IssueServer(hosts, 5*365*24*time.Hour)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	path, h := polarisv1connect.NewAgentServiceHandler(&agentService{s: s})
	mux.Handle(path, withPeerCert(h))
	mux.Handle("/agent/binary/", withPeerCert(http.HandlerFunc(s.serveAgentBinary)))
	mux.Handle("/agent/tool", withPeerCert(http.HandlerFunc(s.serveTool)))
	mux.Handle("/agent/core", withPeerCert(http.HandlerFunc(s.serveCoreRelay)))
	srv := &http.Server{
		Addr:              s.cfg.AgentListen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientCAs:    s.ca.Pool(),
			ClientAuth:   tls.VerifyClientCertIfGiven,
			MinVersion:   tls.VersionTLS12,
		},
	}
	// Ping an idle agent stream: a node that vanished without closing its
	// connection is noticed in about 45 s instead of hanging on.
	if err := http2.ConfigureServer(srv, &http2.Server{ReadIdleTimeout: 30 * time.Second, PingTimeout: 15 * time.Second}); err != nil {
		return nil, err
	}
	return srv, nil
}

// jobCronSpec renders a validated job interval (see validateJobInterval)
// as a robfig cron spec. Sub-minute intervals run on the minute boundary
// ("*/N"); minute-scale intervals keep their second offset so jobs stay
// staggered instead of all firing at :00.
func jobCronSpec(d time.Duration, second int) string {
	// A zero or negative interval must never reach the cron parser as
	// "*/0 * * * * *": AddFunc rejects it and the job silently never
	// runs. applyJobDefaults should have filled it in; fall back to
	// 10s so a missed default degrades to a working schedule.
	if d <= 0 {
		d = 10 * time.Second
	}
	if d < time.Minute {
		return fmt.Sprintf("*/%d * * * * *", int(d/time.Second))
	}
	if d == time.Minute {
		return fmt.Sprintf("%d * * * * *", second)
	}
	return fmt.Sprintf("%d */%d * * * *", second, int(d/time.Minute))
}

// startJobs schedules periodic tasks.
func (s *Server) startJobs() func() {
	c := cron.New(cron.WithSeconds())
	// A job never overlaps with itself; panics are recovered (jobs.go).
	run := func(name string, fn func()) { s.runJob(name, fn) }
	add := func(spec, name string, fn func()) {
		if _, err := c.AddFunc(spec, func() {
			if s.isLeader() { // otherwise another instance runs the scheduled jobs
				run(name, fn)
			}
		}); err != nil {
			// Never ignore this: an invalid spec means the job never runs,
			// and the failure is otherwise completely silent.
			s.log.Error("schedule job failed", "job", name, "spec", spec, "err", err)
		}
	}
	// addLocal runs on every instance: work on this machine's own files.
	addLocal := func(spec, name string, fn func()) {
		if _, err := c.AddFunc(spec, func() { run(name, fn) }); err != nil {
			s.log.Error("schedule job failed", "job", name, "spec", spec, "err", err)
		}
	}
	add(jobCronSpec(s.cfg.Jobs.Settle, 0), "settle", func() { // keeps usage, quotas and the dashboard near real time
		err := s.settleTraffic()
		s.recordOperationalResult("traffic-settlement", "流量结算失败，待结算记录将保留重试", err)
		if err != nil {
			s.log.Error("settle traffic", "err", err)
		}
		// Enforce again even though settleTraffic enforces on success: it
		// returns early without enforcing when there is no traffic, the
		// inbox was taken, or the settlement failed, and time-based expiry
		// must not wait for the next successful settlement. On a successful
		// round this is a second full scan that finds nothing to change.
		s.enforceLimits()
	})
	add(jobCronSpec(s.cfg.Jobs.Offline, 0), "offline", s.markOfflineNodes)
	add(jobCronSpec(s.cfg.Jobs.Webhooks, 0), "webhooks", s.retryWebhooks)
	add("20 * * * * *", "orders", s.expireOrders)
	add(jobCronSpec(s.cfg.Jobs.PaymentRecovery, 0), "payment-recovery", s.retryPayments)
	add(jobCronSpec(s.cfg.Jobs.RefundRecovery, 0), "refund-recovery", s.retryRefunds)
	add(jobCronSpec(s.cfg.Jobs.Rollouts, 0), "rollouts", s.runRollouts)
	add(jobCronSpec(s.cfg.Jobs.UserReminders, 0), "user-reminders", s.retryUserNotices)
	addLocal(jobCronSpec(s.cfg.Jobs.PluginCache, 0), "plugin-cache", s.renderCache.prune)
	add("0 * * * * *", "operational-alerts", s.checkOperationalHealth)
	add("0 */15 * * * *", "secret-migration", s.secretMigrationTick) // resumes a key rotation; no-op when done
	add("40 */5 * * * *", "ephemeral", s.eph.sweep)
	add("0 7 * * * *", "portal-sessions", s.prunePortalSessions)
	add(jobCronSpec(s.cfg.Jobs.USDT, 0), "usdt", s.pollUSDT)
	add("5 * * * * *", "plugins", s.checkPlugins)
	add("0 5 * * * *", "reset", s.resetTrafficCycles)
	add("0 17 3 * * *", "cleanup", s.cleanup)
	add("0 53 3 * * *", "usage-stats", s.reportUsageStats) // one anonymous usage ping a day; no-op when telemetry is off
	add("0 23 4 * * *", "certs", s.renewCertificates)
	add("0 7 */6 * * *", "preferred-ips", s.refreshAllPreferredIPs)
	add("0 37 */6 * * *", "cdn-dns", s.autoSyncCDNDNS)
	add("0 23 5 * * *", "cdn-shodan", s.autoRefreshCDNShodan)
	add("0 13 * * * *", "preferred-ips-stale", s.checkCDNReportsStale)
	add("30 */30 * * * *", "preferred-ips-nodes", s.checkCDNIPsFromNodes)
	add("25 * * * * *", "cdn-address-checks", s.checkScheduledCDNAddresses)
	add("0 */10 * * * *", "ddns", s.syncAllBindings)
	add(jobCronSpec(s.cfg.Jobs.Health, 45), "health", s.probeChains)
	add(jobCronSpec(s.cfg.Jobs.ForwardFailover, 0), "forward-failover", s.probeForwards)
	add("15 * * * * *", "sub-sources", s.refreshDueSources)
	add("0 33 * * * *", "reminders", s.remindUsers)
	add("0 11 * * * *", "license-expiry", s.remindLicenseExpiry)
	add("0 2 * * * *", "backup", s.scheduledBackup)
	add("0 1 * * * *", "daily-report", s.dailyReport)
	add("0 0 * * * *", "auto-update", s.autoUpdateTick)
	addLocal(jobCronSpec(s.cfg.Jobs.UpdateResult, 0), "update-result", s.checkUpdateResult) // each machine updates itself
	add("0 37 * * * *", "traffic-spikes", s.checkTrafficSpikes)
	add(jobCronSpec(s.cfg.Jobs.InboundProbe, 30), "inbound-probe", s.probeInbounds)
	add("0 20 4 * * *", "rule-sets", s.refreshRemoteRuleSets)
	add("0 3 * * * *", "node-traffic-cycles", s.resetNodeCycles)
	add("0 13 9 * * *", "server-renewals", s.costRenewals)
	add("30 */15 * * * *", "providers", s.syncProviders)
	add("0 41 5 * * *", "ticket-autoclose", s.autoCloseTickets)
	add(jobCronSpec(s.cfg.Jobs.LatencyProbes, 0), "latency-probes", s.runLatencyProbes)
	add(jobCronSpec(s.cfg.Jobs.RouteTraces, 0), "route-traces", s.runScheduledRouteTraces)
	if s.isLeader() {
		go s.backfillDaily()
	}
	s.log.Info("scheduled jobs",
		"settle", s.cfg.Jobs.Settle, "payment_recovery", s.cfg.Jobs.PaymentRecovery,
		"latency_probes", s.cfg.Jobs.LatencyProbes, "offline", s.cfg.Jobs.Offline,
		"refund_recovery", s.cfg.Jobs.RefundRecovery, "rollouts", s.cfg.Jobs.Rollouts,
		"user_reminders", s.cfg.Jobs.UserReminders, "forward_failover", s.cfg.Jobs.ForwardFailover,
		"update_result", s.cfg.Jobs.UpdateResult, "webhooks", s.cfg.Jobs.Webhooks,
		"plugin_cache", s.cfg.Jobs.PluginCache, "usdt", s.cfg.Jobs.USDT,
		"route_traces", s.cfg.Jobs.RouteTraces, "health", s.cfg.Jobs.Health,
		"inbound_probe", s.cfg.Jobs.InboundProbe)
	c.Start()
	return func() { <-c.Stop().Done() }
}

func (s *Server) markOfflineNodes() {
	cut := time.Now().UTC().Add(-90 * time.Second)
	var ns []store.Node
	s.st.DB.Where("status IN ? AND (last_seen IS NULL OR last_seen < ?)", []string{"online", "syncing", "error"}, cut).Find(&ns)
	for _, n := range ns {
		if !s.hub.Online(n.ID) {
			s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Update("status", "offline")
			s.notify(Event{Kind: "node_offline", NodeID: n.ID, Text: "🔴 节点 " + n.Name + " 心跳超时，已离线"})
		}
	}
	if len(ns) > 0 {
		s.broadcastNodes()
	}
}

// warnWrite logs a failed best-effort write. These are periodic status
// writes the next run repeats, so a failure is reported, not retried here.
func (s *Server) warnWrite(what string, err error) {
	if err != nil {
		s.log.Warn("database write failed", "what", what, "err", err)
	}
}

func (s *Server) cleanup() {
	now := time.Now().UTC()
	prune := func(what string, model any, where string, args ...any) {
		if err := s.st.DB.Where(where, args...).Delete(model).Error; err != nil {
			s.log.Warn("cleanup", "table", what, "err", err) // retried on the next run
		}
	}
	prune("restore drills", &store.RestoreDrill{}, "started_at < ?", now.AddDate(0, 0, -90))
	prune("user notices", &store.UserNotice{}, "status <> ? AND updated_at < ?", "pending", now.AddDate(0, 0, -30))
	s.pruneIdempotency()
	prune("route traces", &store.NodeRouteTrace{}, "started_at < ?", now.Add(-routeTraceKeep))
	prune("notification states", &store.NotificationState{}, "open = ? AND updated_at < ?", false, now.AddDate(0, 0, -30))
	prune("webhook deliveries", &store.WebhookDelivery{}, "status = ? AND created_at < ?", "success", now.AddDate(0, 0, -7))
	// Failed deliveries stay a month for troubleshooting, then go too.
	prune("failed webhook deliveries", &store.WebhookDelivery{}, "status = ? AND created_at < ?", "failed", now.AddDate(0, 0, -30))
	// Cluster operations expire after a day; keep a week of history, then
	// drop their deliveries first and the operations after.
	expiredOps := s.st.DB.Model(&store.ClusterOperation{}).Select("id").Where("expires_at < ?", now.AddDate(0, 0, -7))
	prune("cluster deliveries", &store.ClusterDelivery{}, "operation_id IN (?)", expiredOps)
	prune("cluster operations", &store.ClusterOperation{}, "expires_at < ?", now.AddDate(0, 0, -7))
	prune("hourly traffic", &store.TrafficHourly{}, "hour < ?", now.AddDate(0, 0, -90))
	prune("traffic batches", &store.TrafficBatchSeen{}, "created_at < ?", now.AddDate(0, 0, -30))
	prune("node metrics", &store.NodeMetric{}, "at < ?", now.AddDate(0, 0, -30))
	prune("latency", &store.NodeLatency{}, "at < ?", now.Add(-latencyKeep))
	prune("external metrics", &store.NodeExternalMetric{}, "at < ?", now.Add(-extMetricsKeep))
	prune("admin sessions", &store.AdminSession{}, "expires_at < ?", now.AddDate(0, 0, -90)) // kept a while: new-address alerts compare against them
	prune("login bans", &store.LoginBan{}, "(until IS NULL OR until < ?) AND updated_at < ?", now, now.AddDate(0, 0, -30))
	prune("audit log", &store.AuditLog{}, "created_at < ?", now.AddDate(0, 0, -180))
}

// audit writes an audit log entry.
func (s *Server) audit(admin, action, target, detail, ip string) {
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	s.st.DB.Create(&store.AuditLog{Admin: admin, Action: action, Target: truncate(target, 250), Detail: truncate(detail, 4000), IP: ip})
}
