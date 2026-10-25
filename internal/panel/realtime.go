package panel

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// realtime fans node snapshots out to WebSocket clients (/api/v1/ws).
type realtime struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]*rtClient
	dirty   atomic.Bool
	snap    func() any
}

// rtClient is one WebSocket stream; scope limits the nodes it sees (nil = all).
type rtClient struct {
	ch    chan []byte
	scope map[string]bool
}

func newRealtime() *realtime { return &realtime{clients: map[*websocket.Conn]*rtClient{}} }

// nodesMessage renders the snapshot for one scope.
func nodesMessage(data any, scope map[string]bool) ([]byte, error) {
	if vs, ok := data.([]nodeView); ok && scope != nil {
		data = scopedNodeViews(vs, scope)
	}
	return json.Marshal(map[string]any{"type": "nodes", "data": data})
}

func (r *realtime) run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !r.dirty.Swap(false) || r.snap == nil {
				continue
			}
			data := r.snap()
			all, err := nodesMessage(data, nil)
			if err != nil {
				continue
			}
			r.mu.Lock()
			for _, cl := range r.clients {
				b := all
				if cl.scope != nil {
					if b, err = nodesMessage(data, cl.scope); err != nil {
						continue
					}
				}
				select {
				case cl.ch <- b:
				default:
				}
			}
			r.mu.Unlock()
		}
	}
}

func (s *Server) broadcastNodes() {
	s.rt.dirty.Store(true)
	s.rtShare.Store(true) // other instances refresh their browsers too
}

// sessionCheck reports, when called later, whether the request's session is
// still valid: a revoked or expired session, a password change, a deleted
// admin or a removed Telegram admin ends it.
func (s *Server) sessionCheck(c *gin.Context) func() bool {
	sid := c.GetString("sid") // empty for API tokens
	alive := func() bool {
		if sid == "" {
			return true
		}
		_, ok := s.sessionAlive(sid)
		return ok
	}
	if v, ok := c.Get("tgAdmin"); ok {
		id := v.(*store.TGAdmin).ID
		return func() bool {
			if !alive() {
				return false
			}
			var n int64
			s.st.DB.Model(&store.TGAdmin{}).Where("id = ?", id).Count(&n)
			return n > 0
		}
	}
	adminID, version := c.GetString("adminID"), c.GetInt("tokenVersion")
	return func() bool {
		var a store.Admin
		return alive() && s.st.DB.Select("id", "token_version").First(&a, "id = ?", adminID).Error == nil && a.TokenVersion == version
	}
}

// handleWS upgrades an authenticated request. The session is re-checked
// every ping and closes the stream once revoked.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request, valid func() bool, scope map[string]bool) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ch := make(chan []byte, 8)
	s.rt.mu.Lock()
	s.rt.clients[c] = &rtClient{ch: ch, scope: scope}
	s.rt.mu.Unlock()
	defer func() {
		s.rt.mu.Lock()
		delete(s.rt.clients, c)
		s.rt.mu.Unlock()
		c.CloseNow()
	}()
	ctx := c.CloseRead(r.Context())
	if b, err := nodesMessage(s.rt.snap(), scope); err == nil {
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-ch:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				return
			}
		case <-ping.C:
			if !valid() {
				c.Close(websocket.StatusPolicyViolation, "session revoked")
				return
			}
			if err := c.Ping(ctx); err != nil {
				return
			}
		}
	}
}

// ipLimiter is a per-IP token bucket used for the subscription endpoint.
type ipLimiter struct {
	mu   sync.Mutex
	per  rate.Limit
	m    map[string]*rate.Limiter
	seen map[string]time.Time
}

func newIPLimiter(perMinute int) *ipLimiter {
	if perMinute <= 0 {
		perMinute = 30
	}
	return &ipLimiter{per: rate.Limit(float64(perMinute) / 60), m: map[string]*rate.Limiter{}, seen: map[string]time.Time{}}
}

func (l *ipLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > 10000 {
		cut := time.Now().Add(-10 * time.Minute)
		for k, t := range l.seen {
			if t.Before(cut) {
				delete(l.m, k)
				delete(l.seen, k)
			}
		}
	}
	lim, ok := l.m[ip]
	if !ok {
		lim = rate.NewLimiter(l.per, 10)
		l.m[ip] = lim
	}
	l.seen[ip] = time.Now()
	return lim.Allow()
}

// trustedProxies is set from the config (trusted_proxies).
var trustedProxies []*net.IPNet

func trustedPeer(ip net.IP) bool {
	if len(trustedProxies) == 0 {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	for _, n := range trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP extracts the caller IP, trusting X-Forwarded-For only from loopback
// / private proxies (Caddy/Nginx in front).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && trustedPeer(ip) {
		// Behind a local reverse proxy: the right-most X-Forwarded-For entry is
		// the one the proxy appended; entries to its left are client-supplied.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if v := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(v) != nil {
				return v
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(xr) != nil {
			return xr
		}
	}
	return host
}
