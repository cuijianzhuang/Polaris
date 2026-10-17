package panel

import (
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// A separate subscription domain (sub_base_url on another host than the
// panel) serves only what users need: subscriptions and the user center.
// Everything else on that host gets the decoy, so the domain handed to
// users (often behind a CDN) never exposes the admin UI or API.

var subHostCache struct {
	sync.Mutex
	host   string
	strict bool
	at     time.Time
}

// subHost returns the subscription domain when it differs from the panel's
// own address and strict mode is on ("" otherwise). Cached for 15 s.
func (s *Server) subHost() string {
	subHostCache.Lock()
	defer subHostCache.Unlock()
	if time.Since(subHostCache.at) < 15*time.Second {
		if subHostCache.strict {
			return subHostCache.host
		}
		return ""
	}
	subHostCache.at = time.Now()
	subHostCache.host, subHostCache.strict = "", false
	u, err := url.Parse(strings.TrimSpace(s.st.GetSetting("sub_base_url", "")))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	if p, err := url.Parse(s.cfg.PublicURL); err == nil && strings.EqualFold(p.Hostname(), h) {
		return "" // same host as the panel: nothing to separate
	}
	subHostCache.host = h
	subHostCache.strict = s.st.GetSetting("sub_host_strict", "1") != "0"
	if subHostCache.strict {
		return h
	}
	return ""
}

func resetSubHostCache() {
	subHostCache.Lock()
	subHostCache.at = time.Time{}
	subHostCache.Unlock()
}

// subOnlyPath lists what a subscription domain serves.
func subOnlyPath(p string) bool {
	return strings.HasPrefix(p, "/sub/") || p == portalPath || strings.HasPrefix(p, portalPath+"/") ||
		p == "/favicon.ico" || p == "/robots.txt"
}

// subHostGate keeps the subscription domain to subscription routes.
func (s *Server) subHostGate(decoy gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := s.subHost()
		if h == "" {
			c.Next()
			return
		}
		host := c.Request.Host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			host = hh
		}
		if !strings.EqualFold(host, h) || subOnlyPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		decoy(c)
		c.Abort()
	}
}
