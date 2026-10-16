// Package panel is the Polaris control plane: REST API, desired-state
// publication, the agent gRPC service, traffic settlement and scheduled jobs.
package panel

import (
	"context"
	"crypto/tls"
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
	cfg         *Config
	st          *store.Store
	ca          *pki.CA
	log         *slog.Logger
	hub         *Hub
	traffic     TrafficBuffer
	tgLogins    tgLoginCodes // one-time Telegram sign-in codes
	tgWeb       tgWebLogins  // login-page Telegram sign-in requests
	hbMu        sync.Mutex
	restartHook func()                 // tests: replaces the re-exec after a restore
	hbCores     map[string]hbCoreState // last core state written per node
	jwtKey      []byte
	rt          *realtime

	dirtyMu sync.Mutex
	dirty   map[string]string
	kick    chan struct{}
	pubMu   sync.Mutex

	onlineMu  sync.Mutex
	onlineIPs map[string]map[string]map[string]time.Time // user -> node -> ip
	alerts    map[string]time.Time

	subLimit     *ipLimiter
	callbacks    callbackStore
	nodeUserSets nodeUsersCache
	schemes      schemeCache
	ghAuth       githubAuth
	webFS        http.FileSystem
	bg           context.Context
	sinks        []chan Event
	health       healthState
	tg           *tgBot
	tgEvents     chan Event
	seenBase     atomic.Value // string: public base URL the admin UI is reached at
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
	if ok, err := applyPendingRestore(cfg); err != nil {
		return nil, fmt.Errorf("apply restored backup: %w", err)
	} else if ok {
		log.Warn("restored database and CA from backup; previous files kept in " + filepath.Join(cfg.DataDir, backupDir))
	}
	st, err := store.Open(cfg.DB.DSN, box, cfg.DB.Debug)
	if err != nil {
		return nil, err
	}
	ca, err := loadOrCreateCA(cfg.DataDir, box)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, st: st, ca: ca, log: log, hub: newHub(),
		jwtKey: []byte("jwt:" + cfg.SecretKey), rt: newRealtime(),
		dirty: map[string]string{}, kick: make(chan struct{}, 1),
		onlineIPs: map[string]map[string]map[string]time.Time{}, alerts: map[string]time.Time{},
		subLimit: newIPLimiter(cfg.SubRateLimit), webFS: web, bg: context.Background(),
	}
	s.rt.snap = func() any { return s.nodeViews() }
	s.tg = newTGBot(s)
	tgEvents := make(chan Event, 256)
	s.sinks = append(s.sinks, tgEvents)
	s.tgEvents = tgEvents
	if cfg.RedisURL != "" {
		rb, err := newRedisBuffer(cfg.RedisURL)
		if err != nil {
			return nil, err
		}
		s.traffic = rb
	} else {
		s.traffic = newMemBuffer()
	}
	if err := s.seed(); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	return s, nil
}

func (s *Server) ctxBG() context.Context { return s.bg }

// Store exposes the repository (used by tests and the CLI).
func (s *Server) Store() *store.Store { return s.st }

func loadOrCreateCA(dir string, box *secret.Box) (*pki.CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	certPEM, err1 := os.ReadFile(certPath)
	keySealed, err2 := os.ReadFile(keyPath)
	if err1 == nil && err2 == nil {
		keyPEM, err := box.Open(string(keySealed))
		if err != nil {
			return nil, fmt.Errorf("decrypt CA key (wrong secret_key?): %w", err)
		}
		return pki.LoadCA(certPEM, []byte(keyPEM))
	}
	ca, err := pki.NewCA("Polaris Internal CA")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, ca.CertPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, []byte(box.Seal(string(ca.KeyPEM))), 0o600); err != nil {
		return nil, err
	}
	return ca, nil
}

// Run serves HTTP and the agent endpoint until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.bg = ctx
	go s.runReconciler(ctx)
	go s.runAgentSync(ctx)
	go s.rt.run(ctx)
	go s.tg.run(ctx)
	go s.tg.runNotifications(ctx, s.tgEvents)
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
	if err := http2.ConfigureServer(srv, &http2.Server{}); err != nil {
		return nil, err
	}
	return srv, nil
}

// startJobs schedules periodic tasks.
func (s *Server) startJobs() func() {
	c := cron.New(cron.WithSeconds())
	add := func(spec, name string, fn func()) {
		_, _ = c.AddFunc(spec, func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("job panicked", "job", name, "panic", r)
				}
			}()
			fn()
		})
	}
	add("*/10 * * * * *", "settle", func() { // keeps usage, quotas and the dashboard near real time
		if err := s.settleTraffic(); err != nil {
			s.log.Error("settle traffic", "err", err)
		}
		s.enforceLimits()
	})
	add("*/15 * * * * *", "offline", s.markOfflineNodes)
	add("0 5 * * * *", "reset", s.resetTrafficCycles)
	add("0 17 3 * * *", "cleanup", s.cleanup)
	add("0 23 4 * * *", "certs", s.renewCertificates)
	add("0 7 */6 * * *", "preferred-ips", s.refreshAllPreferredIPs)
	add("0 */10 * * * *", "ddns", s.syncAllBindings)
	add("45 * * * * *", "health", s.probeChains)
	add("*/20 * * * * *", "forward-failover", s.probeForwards)
	add("15 * * * * *", "sub-sources", s.refreshDueSources)
	add("0 33 * * * *", "reminders", s.remindUsers)
	add("0 2 * * * *", "backup", s.scheduledBackup)
	add("0 1 * * * *", "daily-report", s.dailyReport)
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
	s.st.DB.Where("hour < ?", time.Now().AddDate(0, 0, -90)).Delete(&store.TrafficHourly{})
	s.st.DB.Where("created_at < ?", time.Now().AddDate(0, 0, -180)).Delete(&store.AuditLog{})
}

// audit writes an audit log entry.
func (s *Server) audit(admin, action, target, detail, ip string) {
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	s.st.DB.Create(&store.AuditLog{Admin: admin, Action: action, Target: truncate(target, 250), Detail: truncate(detail, 4000), IP: ip})
}
