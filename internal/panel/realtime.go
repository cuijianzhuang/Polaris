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

	"github.com/coder/websocket"
	"golang.org/x/time/rate"
)

// realtime fans node snapshots out to WebSocket clients (/api/v1/ws).
type realtime struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]chan []byte
	dirty   atomic.Bool
	snap    func() any
}

func newRealtime() *realtime { return &realtime{clients: map[*websocket.Conn]chan []byte{}} }

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
			b, err := json.Marshal(map[string]any{"type": "nodes", "data": r.snap()})
			if err != nil {
				continue
			}
			r.mu.Lock()
			for _, ch := range r.clients {
				select {
				case ch <- b:
				default:
				}
			}
			r.mu.Unlock()
		}
	}
}

func (s *Server) broadcastNodes() { s.rt.dirty.Store(true) }

// handleWS upgrades an authenticated request.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ch := make(chan []byte, 8)
	s.rt.mu.Lock()
	s.rt.clients[c] = ch
	s.rt.mu.Unlock()
	defer func() {
		s.rt.mu.Lock()
		delete(s.rt.clients, c)
		s.rt.mu.Unlock()
		c.CloseNow()
	}()
	ctx := c.CloseRead(r.Context())
	if b, err := json.Marshal(map[string]any{"type": "nodes", "data": s.rt.snap()}); err == nil {
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

// clientIP extracts the caller IP, trusting X-Forwarded-For only from loopback
// / private proxies (Caddy/Nginx in front).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
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
