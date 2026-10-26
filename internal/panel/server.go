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
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"

	// Register every core adapter so capabilities are available.
	_ "github.com/cuijianzhuang/polaris/internal/core/caddy"
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
	renderCache    renderCache // plugin subscription output (plugin_sub.go)
	userNoticeBusy atomic.Bool
	cfg            *Config
	st             *store.Store
	ca             *pki.CA
	log            *slog.Logger
	hub            *Hub
	traffic        *trafficInbox
	eph            *ephStore   // short-lived values shared by every instance (sign-in codes, OAuth states)
	tgWeb          tgWebLogins // login-page Telegram sign-in requests
	portalWeb      tgWebLogins // user center Telegram sign-in requests
	hbMu           sync.Mutex
	probeBusy      atomic.Bool                         // a probe round is running
	statusCache    atomic.Pointer[statusSnap]          // public status page data
	restartHook    func()                              // tests: replaces the re-exec after a restore
	hbCores        map[string]hbCoreState              // last core state written per node
	progress       map[string]*nodeProgress            // live agent activity per node (guarded by hbMu)
	limited        map[string][]*polarisv1.LimitedUser // per-user speed limits in effect per node (hbMu)
	lic            atomic.Pointer[licenseState]        // license in effect (see license.go)
	licKeys        []ed25519.PublicKey                 // overrides the built-in license keys (tests)
	jwtKey         []byte
	rt             *realtime

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
	routeGeoCache     *routeGeoDatabase
	routeASNMu        sync.Mutex
	routeASNCache     *routeASNDatabase
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
	if cfg.AdminPath == "/" {
		log.Warn("admin UI is served at / — set POLARIS_ADMIN_PATH to a hard-to-guess path so scanners only see 404 or the decoy site")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	box, err := secret.New(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DB.DSN, box, cfg.DB.Debug)
	if err != nil {
		return nil, err
	}
	restored := applyStagedRestore(st, cfg, box, log) // a backup chosen in the UI, before anything reads the data
	ca, err := loadOrCreateCA(st, cfg.DataDir, box)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, st: st, ca: ca, log: log, hub: newHub(),
		jwtKey: []byte("jwt:" + cfg.SecretKey), rt: newRealtime(),
		dirty: map[string]string{}, kick: make(chan struct{}, 1),
		webFS: web, bg: context.Background(),
	}
	s.rt.snap = func() any { return s.nodeViews() }
	s.tg = newTGBot(s)
	s.auditChanges.Store(st.GetSetting("audit_changes", "false") == "true")
	s.registerChangeLog()
	tgEvents := make(chan Event, 256)
	s.sinks = append(s.sinks, tgEvents)
	s.tgEvents = tgEvents
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
func loadOrCreateCA(st *store.Store, dir string, box *secret.Box) (*pki.CA, error) {
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

	httpSrv := &http.Server{Addr: s.cfg.Listen, Handler: s.Router(), ReadHeaderTimeout: 10 * time.Second}

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

// startJobs schedules periodic tasks.
func (s *Server) startJobs() func() {
	c := cron.New(cron.WithSeconds())
	run := func(name string, fn func()) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("job panicked", "job", name, "panic", r)
			}
		}()
		fn()
	}
	add := func(spec, name string, fn func()) {
		_, _ = c.AddFunc(spec, func() {
			if s.isLeader() { // otherwise another instance runs the scheduled jobs
				run(name, fn)
			}
		})
	}
	// addLocal runs on every instance: work on this machine's own files.
	addLocal := func(spec, name string, fn func()) {
		_, _ = c.AddFunc(spec, func() { run(name, fn) })
	}
	add("*/10 * * * * *", "settle", func() { // keeps usage, quotas and the dashboard near real time
		err := s.settleTraffic()
		s.recordOperationalResult("traffic-settlement", "流量结算失败，待结算记录将保留重试", err)
		if err != nil {
			s.log.Error("settle traffic", "err", err)
		}
		s.enforceLimits()
	})
	add("*/15 * * * * *", "offline", s.markOfflineNodes)
	add("*/30 * * * * *", "webhooks", s.retryWebhooks)
	add("20 * * * * *", "orders", s.expireOrders)
	add("*/10 * * * * *", "payment-recovery", s.retryPayments)
	add("*/15 * * * * *", "refund-recovery", s.retryRefunds)
	add("*/15 * * * * *", "rollouts", s.runRollouts)
	add("*/15 * * * * *", "user-reminders", s.retryUserNotices)
	addLocal("*/30 * * * * *", "plugin-cache", s.renderCache.prune)
	add("0 * * * * *", "operational-alerts", s.checkOperationalHealth)
	add("40 */5 * * * *", "ephemeral", s.eph.sweep)
	add("*/30 * * * * *", "usdt", s.pollUSDT)
	add("5 * * * * *", "plugins", s.checkPlugins)
	add("0 5 * * * *", "reset", s.resetTrafficCycles)
	add("0 17 3 * * *", "cleanup", s.cleanup)
	add("0 23 4 * * *", "certs", s.renewCertificates)
	add("0 7 */6 * * *", "preferred-ips", s.refreshAllPreferredIPs)
	add("0 */10 * * * *", "ddns", s.syncAllBindings)
	add("45 * * * * *", "health", s.probeChains)
	add("*/20 * * * * *", "forward-failover", s.probeForwards)
	add("15 * * * * *", "sub-sources", s.refreshDueSources)
	add("0 33 * * * *", "reminders", s.remindUsers)
	add("0 11 * * * *", "license-expiry", s.remindLicenseExpiry)
	add("0 2 * * * *", "backup", s.scheduledBackup)
	add("0 1 * * * *", "daily-report", s.dailyReport)
	add("0 0 * * * *", "auto-update", s.autoUpdateTick)
	addLocal("*/20 * * * * *", "update-result", s.checkUpdateResult) // each machine updates itself
	add("0 37 * * * *", "traffic-spikes", s.checkTrafficSpikes)
	add("30 */2 * * * *", "inbound-probe", s.probeInbounds)
	add("0 20 4 * * *", "rule-sets", s.refreshRemoteRuleSets)
	add("0 3 * * * *", "node-traffic-cycles", s.resetNodeCycles)
	add("0 13 9 * * *", "server-renewals", s.costRenewals)
	add("30 */15 * * * *", "providers", s.syncProviders)
	add("0 41 5 * * *", "ticket-autoclose", s.autoCloseTickets)
	add("*/30 * * * * *", "latency-probes", s.runLatencyProbes)
	add("*/30 * * * * *", "route-traces", s.runScheduledRouteTraces)
	if s.isLeader() {
		go s.backfillDaily()
	}
	c.Start()
	return func() { <-c.Stop().Done() }
}

func (s *Server) markOfflineNodes() {
	cut := time.Now().Add(-90 * time.Second)
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

func (s *Server) cleanup() {
	s.st.DB.Where("status <> ? AND updated_at < ?", "pending", time.Now().AddDate(0, 0, -30)).Delete(&store.UserNotice{})
	s.pruneIdempotency()
	s.st.DB.Where("started_at < ?", time.Now().Add(-routeTraceKeep)).Delete(&store.NodeRouteTrace{})
	s.st.DB.Where("open = ? AND updated_at < ?", false, time.Now().AddDate(0, 0, -30)).Delete(&store.NotificationState{})
	s.st.DB.Where("created_at < ?", time.Now().AddDate(0, 0, -7)).Delete(&store.WebhookDelivery{})
	s.st.DB.Where("hour < ?", time.Now().AddDate(0, 0, -90)).Delete(&store.TrafficHourly{})
	s.st.DB.Where("created_at < ?", time.Now().AddDate(0, 0, -30)).Delete(&store.TrafficBatchSeen{})
	s.st.DB.Where("at < ?", time.Now().AddDate(0, 0, -30)).Delete(&store.NodeMetric{})
	s.st.DB.Where("at < ?", time.Now().Add(-latencyKeep)).Delete(&store.NodeLatency{})
	s.st.DB.Where("at < ?", time.Now().Add(-extMetricsKeep)).Delete(&store.NodeExternalMetric{})
	s.st.DB.Where("expires_at < ?", time.Now().AddDate(0, 0, -90)).Delete(&store.AdminSession{}) // kept a while: new-address alerts compare against them
	s.st.DB.Where("(until IS NULL OR until < ?) AND updated_at < ?", time.Now(), time.Now().AddDate(0, 0, -30)).Delete(&store.LoginBan{})
	s.st.DB.Where("created_at < ?", time.Now().AddDate(0, 0, -180)).Delete(&store.AuditLog{})
}

// audit writes an audit log entry.
func (s *Server) audit(admin, action, target, detail, ip string) {
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	s.st.DB.Create(&store.AuditLog{Admin: admin, Action: action, Target: truncate(target, 250), Detail: truncate(detail, 4000), IP: ip})
}
