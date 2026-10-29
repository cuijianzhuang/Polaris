package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/realityscan"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Third-party reverse proxies in front of Cloudflare, found with Shodan, as
// extra candidates for the speed-test points. The panel only lists them:
// a point tests them from the users' side together with Cloudflare's own
// ranges and reports those that serve the CDN domain and are fast, so a
// dead or slow proxy never reaches a subscription. Off by default: traffic
// through such an address passes a stranger's machine (still TLS to
// Cloudflare end to end, but the operator sees the SNI and volume).

const (
	cdnShodanDefaultQuery = `"server: cloudflare" port:443 -org:"Cloudflare" country:HK,SG,JP,KR,TW`
	cdnShodanMax          = 300
)

// cloudflareRanges are Cloudflare's published ranges (cloudflare.com/ips);
// an address outside them is a third party's.
var cloudflareRanges = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18",
		"108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15",
		"104.16.0.0/13", "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
		"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32", "2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// cloudflareOwned reports whether ip is in Cloudflare's own ranges; ok is
// false for something that is not an IP (a typed-in domain).
func cloudflareOwned(ip string) (owned, ok bool) {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false, false
	}
	a = a.Unmap()
	for _, p := range cloudflareRanges {
		if p.Contains(a) {
			return true, true
		}
	}
	return false, true
}

// thirdPartyIPs lists the addresses of a list that are not Cloudflare's.
func thirdPartyIPs(list []string) []string {
	out := []string{}
	for _, ip := range list {
		if owned, ok := cloudflareOwned(ip); ok && !owned {
			out = append(out, ip)
		}
	}
	return out
}

type cdnShodanState struct {
	IPs   []string  `json:"ips"`
	Total int       `json:"total"`
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

func (s *Server) cdnShodanState() cdnShodanState {
	var st cdnShodanState
	_ = json.Unmarshal([]byte(s.st.GetSetting("cdn_shodan_state", "{}")), &st)
	if st.IPs == nil {
		st.IPs = []string{}
	}
	return st
}

func (s *Server) cdnShodanOn() bool { return s.st.GetSetting("cdn_shodan_enabled", "") == "true" }

func (s *Server) cdnShodanQuery() string {
	return orDefault(strings.TrimSpace(s.st.GetSetting("cdn_shodan_query", "")), cdnShodanDefaultQuery)
}

func (s *Server) cdnShodanPages() int {
	n, _ := strconv.Atoi(s.st.GetSetting("cdn_shodan_pages", "1"))
	return min(max(n, 1), realityscan.MaxShodanPages)
}

// refreshCDNShodan searches Shodan and keeps the third-party addresses; a
// failed search keeps the last list and records the error.
func (s *Server) refreshCDNShodan(ctx context.Context) cdnShodanState {
	st := s.cdnShodanState()
	key := s.st.Box.MustOpen(s.st.GetSetting("shodan_api_key", ""))
	ips, total, err := realityscan.ShodanIPs(ctx, key, s.cdnShodanQuery(), s.cdnShodanPages())
	st.At = time.Now()
	if err != nil {
		st.Error = err.Error()
	} else {
		st.Error, st.Total = "", total
		kept := []string{}
		for _, ip := range ips {
			if owned, _ := cloudflareOwned(ip); !owned && len(kept) < cdnShodanMax {
				kept = append(kept, ip)
			}
		}
		st.IPs = kept
	}
	b, _ := json.Marshal(st)
	s.warnWrite("cdn shodan state", s.st.SetSetting("cdn_shodan_state", string(b)))
	return st
}

// autoRefreshCDNShodan is the daily search, while the source is on.
func (s *Server) autoRefreshCDNShodan() {
	if !s.cdnShodanOn() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s.refreshCDNShodan(ctx)
}

func (s *Server) cdnShodanView() gin.H {
	st := s.cdnShodanState()
	return gin.H{"enabled": s.cdnShodanOn(), "query": s.cdnShodanQuery(), "defaultQuery": cdnShodanDefaultQuery,
		"pages": s.cdnShodanPages(), "keySet": s.st.GetSetting("shodan_api_key", "") != "",
		"count": len(st.IPs), "total": st.Total, "at": st.At, "error": st.Error, "sample": st.IPs[:min(len(st.IPs), 20)]}
}

// getCDNShodan is GET /cdn-shodan.
func (s *Server) getCDNShodan(c *gin.Context) { c.JSON(http.StatusOK, s.cdnShodanView()) }

// putCDNShodan is PUT /cdn-shodan {enabled, query, pages}; turning it on
// (or changing the search) searches right away.
func (s *Server) putCDNShodan(c *gin.Context) {
	var req struct {
		Enabled bool   `json:"enabled"`
		Query   string `json:"query"`
		Pages   int    `json:"pages"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	req.Query = strings.TrimSpace(req.Query)
	if len(req.Query) > 500 {
		badRequest(c, errors.New("查询条件最长 500 个字符"))
		return
	}
	if req.Query == cdnShodanDefaultQuery {
		req.Query = ""
	}
	if req.Enabled && s.st.GetSetting("shodan_api_key", "") == "" {
		badRequest(c, errors.New("请先在「系统设置」填写 Shodan API Key"))
		return
	}
	changed := req.Query != s.st.GetSetting("cdn_shodan_query", "") || strconv.Itoa(min(max(req.Pages, 1), realityscan.MaxShodanPages)) != strconv.Itoa(s.cdnShodanPages())
	turnedOn := req.Enabled && !s.cdnShodanOn()
	for k, v := range map[string]string{"cdn_shodan_enabled": strconv.FormatBool(req.Enabled), "cdn_shodan_query": req.Query,
		"cdn_shodan_pages": strconv.Itoa(min(max(req.Pages, 1), realityscan.MaxShodanPages))} {
		if err := s.st.SetSetting(k, v); err != nil {
			dbError(c, err)
			return
		}
	}
	if req.Enabled && (turnedOn || changed) {
		s.refreshCDNShodan(c.Request.Context())
	}
	s.auditCtx(c, "cdn.shodan", strconv.FormatBool(req.Enabled), s.cdnShodanQuery())
	c.JSON(http.StatusOK, s.cdnShodanView())
}

// refreshCDNShodanNow is POST /cdn-shodan/refresh.
func (s *Server) refreshCDNShodanNow(c *gin.Context) {
	if !s.cdnShodanOn() {
		badRequest(c, errors.New("先开启「第三方反代 IP」"))
		return
	}
	s.refreshCDNShodan(c.Request.Context())
	c.JSON(http.StatusOK, s.cdnShodanView())
}

// cdnIPCandidates is GET /cdn-profiles/:id/ip-candidates: the extra
// addresses a speed-test point tests besides Cloudflare's ranges (scope
// cdn:ips:write). Empty while the Shodan source is off.
func (s *Server) cdnIPCandidates(c *gin.Context) {
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if t := apiTokenOf(c); t != nil && t.CDNProfileID != "" && t.CDNProfileID != p.ID {
		c.JSON(http.StatusForbidden, gin.H{"error": "这个令牌属于另一个 CDN 配置档的测速点"})
		return
	}
	ips := []string{}
	if s.cdnShodanOn() && p.Provider == "cloudflare" {
		ips = s.cdnShodanState().IPs
	}
	c.JSON(http.StatusOK, gin.H{"ips": ips})
}
