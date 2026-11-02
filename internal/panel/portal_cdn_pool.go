package panel

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func personalCDNAllowed(pool store.PersonalCDNPool, address string, port int) bool {
	if slices.Contains(pool.Disabled, address) {
		return false
	}
	check, exists := pool.Checks[net.JoinHostPort(address, strconv.Itoa(port))]
	return !exists || check.OK
}
func personalCDNAddresses(a cdnAudience, profile string, port int) []string {
	pool := a.Pools[profile]
	list := slices.Clone(a.Personal[profile])
	for _, address := range pool.Automatic[strconv.Itoa(port)] {
		if !slices.Contains(list, address) {
			list = append(list, address)
		}
	}
	return slices.DeleteFunc(list, func(address string) bool { return !personalCDNAllowed(pool, address, port) })
}

// All writers reload the latest account under the report lock, so report refreshes
// cannot overwrite a concurrent edit, check or disable action.
func (s *Server) mutatePersonalCDN(user, profile string, change func(*gorm.DB, *store.PersonalCDNPool) error) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		var u store.User
		if err := tx.First(&u, "id = ?", user).Error; err != nil {
			return err
		}
		pools := u.CDNPool.V
		if pools == nil {
			pools = map[string]store.PersonalCDNPool{}
		}
		pool := pools[profile]
		if err := change(tx, &pool); err != nil {
			return err
		}
		pools[profile] = pool
		return tx.Model(&u).Update("cdn_pool", store.NewJSON(pools)).Error
	})
}

// Uses the official probe's existing API protocol; only the account-owned JSON
// pool is changed. Personal reports never reach the public CDNIPReport table.
func (s *Server) personalCDNReport(c *gin.Context, token *store.APIToken) {
	var req cdnIPReportReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := cdnTokenAllows(token, c.Param("id"), req.Source); err != nil {
		c.JSON(403, gin.H{"error": err.Error()})
		return
	}
	if req.Port != 0 && req.Port != token.CDNPort {
		badRequest(c, fmt.Errorf("测速端口与令牌不一致"))
		return
	}
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", token.CDNProfileID).Error; err != nil {
		dbError(c, err)
		return
	}
	req.Port = token.CDNPort
	if req.Scheme == "" {
		req.Scheme = cdnScheme(&p)
	}
	if _, _, err := cdnReportTarget(req, &p); err != nil {
		badRequest(c, err)
		return
	}
	results, err := checkCDNReport(req)
	if err != nil {
		badRequest(c, err)
		return
	}
	addresses := []string{}
	for _, r := range results {
		if r.Loss < 1 && len(addresses) < 32 {
			addresses = append(addresses, r.IP)
		}
	}
	err = s.mutatePersonalCDN(token.CDNUserID, p.ID, func(tx *gorm.DB, pool *store.PersonalCDNPool) error {
		// Pair/reset/revoke uses the same lock. A request authenticated before a
		// reset must not finish writing after its credential has been revoked.
		var current store.APIToken
		if err := tx.First(&current, "id = ? AND hash = ?", token.ID, token.Hash).Error; err != nil {
			return err
		}
		if pool.Automatic == nil {
			pool.Automatic = map[string][]string{}
		}
		pool.Automatic[strconv.Itoa(req.Port)] = addresses
		now := time.Now()
		pool.ReportedAt = &now
		return nil
	})
	if err != nil {
		dbError(c, err)
		return
	}
	c.JSON(200, gin.H{"stored": len(addresses), "preferredIps": addresses})
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

// Portal writes require the signed-in session and CSRF header, and always derive
// ownership from that session rather than accepting a user ID from the client.
func (s *Server) portalCDNPool(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(401, gin.H{"error": "请重新登录"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var req struct {
		ProfileID string `json:"profileId"`
		Action    string `json:"action"`
		Address   string `json:"address"`
		Port      int    `json:"port"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	profiles, err := s.cdnUserProfiles(u)
	if err != nil {
		dbError(c, err)
		return
	}
	i := slices.IndexFunc(profiles, func(p store.CDNProfile) bool { return p.ID == req.ProfileID })
	if i < 0 {
		c.JSON(403, gin.H{"error": "只能维护本人有权限使用的 CDN 配置"})
		return
	}
	p := profiles[i]
	if req.Port == 0 {
		req.Port = cdnDefaultPort(&p)
	}
	if !slices.Contains(s.cdnCheckPorts(&p), req.Port) {
		badRequest(c, fmt.Errorf("请选择当前入口使用的端口"))
		return
	}
	response := gin.H{"ok": true}
	apiBase := strings.TrimRight(s.panelBase(), "/") + s.cfg.APIBase()
	switch req.Action {
	case "pair", "revoke":
		if req.Action == "pair" && (p.Provider != "cloudflare") {
			badRequest(c, fmt.Errorf("官方 cf-ip-probe 仅支持 Cloudflare 优选"))
			return
		}
		err = s.st.DB.Transaction(func(tx *gorm.DB) error {
			if err := lockCDNReports(tx); err != nil {
				return err
			}
			if err := tx.Where("cdn_user_id = ? AND cdn_profile_id = ?", u.ID, p.ID).Delete(&store.APIToken{}).Error; err != nil {
				return err
			}
			if req.Action == "revoke" {
				return nil
			}
			source := "personal"
			_, secret, err := s.issueAPITokenIn(tx, "cf-ip-probe personal", "", []string{ScopeCDNIPs}, 0, "user:"+u.ID, func(t *store.APIToken) {
				t.CDNUserID, t.CDNProfileID, t.CDNSource, t.CDNPort = u.ID, p.ID, source, req.Port
			})
			if err != nil {
				return err
			}
			instance := fmt.Sprintf("personal-%x", sha256.Sum256([]byte(u.ID+"/"+p.ID)))[:25]
			script := "https://github.com/cuijianzhuang/polaris-plugins/releases/latest/download/install.sh"
			response["command"] = "curl -fsSL " + shellQuote(script) + " | sudo sh -s -- --api " + shellQuote(apiBase) +
				" --profile " + shellQuote(p.ID) + " --host " + shellQuote(p.OriginDomain) + " --token " + shellQuote(secret) + " --source personal --instance " + shellQuote(instance) + " --args " + shellQuote("-scheme "+cdnScheme(&p)+" -port "+strconv.Itoa(req.Port)+" -interval 1h")
			return nil
		})
	case "check":
		if !s.subLimit.Allow("personal-cdn-check:" + u.ID) {
			c.JSON(429, gin.H{"error": "检测过于频繁，请稍后再试"})
			return
		}
		normalized, e := normalizeCDNPinnedHosts([]string{req.Address})
		if e != nil || len(normalized) != 1 {
			badRequest(c, fmt.Errorf("请提供有效地址"))
			return
		}
		address := normalized[0]
		candidates := append(slices.Clone(u.CDNPreferred.V[p.ID]), u.CDNPool.V[p.ID].Automatic[strconv.Itoa(req.Port)]...)
		if !slices.Contains(candidates, address) {
			badRequest(c, fmt.Errorf("请先保存该地址"))
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
		defer cancel()
		result := checkPersonalCDN(ctx, address, req.Port, cdnScheme(&p), p.OriginDomain)
		err = s.mutatePersonalCDN(u.ID, p.ID, func(_ *gorm.DB, pool *store.PersonalCDNPool) error {
			if pool.Checks == nil {
				pool.Checks = map[string]store.PersonalCDNCheck{}
			}
			pool.Checks[net.JoinHostPort(address, strconv.Itoa(req.Port))] = result
			return nil
		})
		response["check"] = result
	default:
		badRequest(c, fmt.Errorf("不支持的操作"))
		return
	}
	if err != nil {
		dbError(c, err)
		return
	}
	s.audit(portalActor(u, s.portalViaMail(c)), "user.cdn_pool."+req.Action, u.Username, p.ID, clientIP(c.Request))
	c.JSON(http.StatusOK, response)
}

// Check connectivity from the panel. The socket guard validates the resolved
// destination, preventing private DNS answers and DNS rebinding from reaching LANs.
func checkPersonalCDN(ctx context.Context, address string, port int, scheme, host string) store.PersonalCDNCheck {
	r := store.PersonalCDNCheck{At: time.Now()}
	dialer := &net.Dialer{Timeout: 8 * time.Second, Control: func(_, destination string, _ syscall.RawConn) error {
		ip, _, err := net.SplitHostPort(destination)
		if err != nil {
			return err
		}
		return checkSourceIP(net.ParseIP(ip), false)
	}}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
	if err == nil {
		defer conn.Close()
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		if scheme == "https" {
			err = tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}).HandshakeContext(ctx)
		}
	}
	if err != nil {
		r.Message = "连接检测失败：" + err.Error()
		return r
	}
	r.OK = true
	r.Message = "TCP／TLS 连接检测通过；测速以本地 cf-ip-probe 结果为准"
	return r
}
