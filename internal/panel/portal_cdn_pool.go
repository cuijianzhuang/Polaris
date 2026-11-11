package panel

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func personalCDNAllowed(pool store.PersonalCDNPool, address string, port int) bool {
	if slices.Contains(pool.Disabled, address) {
		return false
	}
	return true // Legacy port-only checks cannot identify an actual entry.
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
		if pool.PortReports == nil {
			pool.PortReports = map[string]store.PersonalCDNReportMeta{}
		}
		// Freeze legacy timestamps before advancing the profile-wide timestamp.
		for oldPort := range pool.Automatic {
			if _, exists := pool.PortReports[oldPort]; !exists && pool.ReportedAt != nil {
				pool.PortReports[oldPort] = store.PersonalCDNReportMeta{At: *pool.ReportedAt}
			}
		}
		pool.PortReports[strconv.Itoa(req.Port)] = store.PersonalCDNReportMeta{At: time.Now().UTC(), Scheme: req.Scheme}
		pool.Automatic[strconv.Itoa(req.Port)] = addresses
		now := time.Now().UTC()
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
		ProfileID  string `json:"profileId"`
		EndpointID string `json:"endpointId"`
		Context    string `json:"context"`
		Action     string `json:"action"`
		Address    string `json:"address"`
		Port       int    `json:"port"`
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
	entries, err := s.personalCDNEntries(s.st.DB, u, &p)
	if err != nil {
		dbError(c, err)
		return
	}
	if req.Action == "preview" {
		s.personalCDNPreviewResponse(c, u, &p, entries)
		return
	}

	if !slices.ContainsFunc(entries, func(entry personalCDNEntry) bool { return entry.Target.Port == req.Port }) {
		badRequest(c, fmt.Errorf("请选择本人有权限使用的入口端口"))
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
				" --profile " + shellQuote(p.ID) + " --host " + shellQuote(personalCDNProbeHost(entries, req.EndpointID, req.Port, p.OriginDomain)) + " --token " + shellQuote(secret) + " --source personal --instance " + shellQuote(instance) + " --args " + shellQuote("-scheme "+cdnScheme(&p)+" -port "+strconv.Itoa(req.Port)+" -interval 1h")
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
		entryIndex := slices.IndexFunc(entries, func(entry personalCDNEntry) bool {
			return entry.EndpointID == req.EndpointID && entry.Target.Port == req.Port
		})
		if entryIndex < 0 {
			c.JSON(403, gin.H{"error": "请选择本人有权限使用的检测入口"})
			return
		}
		entry := entries[entryIndex]
		if req.Context != "" && req.Context != entry.Context {
			c.JSON(409, gin.H{"error": "入口配置已改变，请刷新后重新检测"})
			return
		}
		target := entry.Target
		target.Server = address
		ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
		defer cancel()
		at := time.Now()
		checked := probeCDNWithDialer(ctx, target, entry.Transport, "", personalCDNDialer())
		if c.Request.Context().Err() != nil {
			return
		} // A disconnected client is not evidence of an unreachable address.
		result := store.PersonalCDNCheck{At: at, OK: checked.OK, Partial: checked.Partial, Message: checked.Message, Fingerprint: cdnCheckKey(entry.EndpointID, target, entry.Transport, entry.Revision)}
		err = s.mutatePersonalCDN(u.ID, p.ID, func(_ *gorm.DB, pool *store.PersonalCDNPool) error {
			result = recordPersonalCDNVerdict(pool, entry.EndpointID, address, result)

			return nil
		})
		response["endpointId"] = entry.EndpointID
		response["stages"] = checked.Stages

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
