package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/panel/cdnkit"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Restrict personal choices to profiles reached through this user's grants.
func (s *Server) cdnUserProfiles(u *store.User) ([]store.CDNProfile, error) {
	var ids []string
	err := s.st.DB.Table("endpoints").Distinct("endpoints.cdn_profile_id").
		Joins("JOIN inbounds ON inbounds.id = endpoints.inbound_id").
		Joins("JOIN nodes ON nodes.id = inbounds.node_id").
		Joins("JOIN ("+store.GrantsSQL+") grants ON grants.inbound_id = inbounds.id").
		Where("grants.user_id = ? AND endpoints.kind = ? AND inbounds.enabled = ? AND inbounds.service_only = ? AND nodes.revoked = ?", u.ID, "cdn", true, false, false).
		Pluck("endpoints.cdn_profile_id", &ids).Error
	if err != nil {
		return nil, err
	}
	var profiles []store.CDNProfile
	if len(ids) == 0 {
		return profiles, nil
	}
	err = s.st.DB.Where("id IN ?", ids).Order("name,id").Find(&profiles).Error
	return profiles, err
}

func (s *Server) cdnSelectionText(p *store.CDNProfile, carrier, region string, personal []string, ports ...int) string {
	port := cdnkit.DefaultPort(p)
	if len(ports) > 0 {
		port = ports[0]
	}
	if !p.ExpandPerIP {
		return "未开启订阅展开，当前使用入口指定地址或回源域名"
	}
	if len(personal) > 0 {
		available := cdnkit.UsableIPsAt(p, personal, port)
		return fmt.Sprintf("个人优选：%d 个手动候选，%d 个通过公共规则筛选；实际订阅同时过滤个人禁用和检测失败的地址，无可用个人地址时使用公共池", len(personal), len(available))
	}
	_, mode := s.cdnAudienceReports(p, port, carrier, region)
	switch mode {
	case "region_carrier":
		return "已匹配同地区、同运营商的测速结果"
	case "region":
		return "已匹配同地区的测速结果；运营商未指定或未启用分配"
	case "carrier":
		if p.RegionSplit && region != "" {
			return "本地区没有匹配结果，已回退到同运营商其他地区"
		}
		return "使用同运营商的测速结果"
	default:
		return "没有匹配的分组结果，使用合并或保留列表"
	}
}
func (s *Server) portalCDNInfo(info *sub.Info, u *store.User, carrier, region string) error {
	info.CDNManage = true
	info.RegionPick = s.cdnRegionSplitOn()
	info.Region = u.CDNRegion
	for _, o := range s.cdnRegionOptions() {
		info.CDNRegions = append(info.CDNRegions, sub.CDNRegionChoice{Code: o.Code, Name: o.Name})
	}
	info.CDNNetwork = "本次共享选择：地区 " + orDefault(cdnkit.RegionName(region), "未识别") + " · 运营商 " + orDefault(cdnkit.CarrierName(carrier), "未识别")
	profiles, err := s.cdnUserProfiles(u)
	if err != nil {
		return err
	}
	for _, p := range profiles {
		personal := u.CDNPreferred.V[p.ID]
		poolJSON, _ := json.Marshal(u.CDNPool.V[p.ID])
		entries, err := s.personalCDNEntries(s.st.DB, u, &p)
		if err != nil {
			return err
		}
		entriesJSON, _ := json.Marshal(entries)
		ports := []int{}
		for _, entry := range entries {
			if !slices.Contains(ports, entry.Target.Port) {
				ports = append(ports, entry.Target.Port)
			}
		}
		if index := slices.Index(ports, cdnkit.DefaultPort(&p)); index >= 0 {
			ports[0], ports[index] = ports[index], ports[0]
		}
		portsJSON, _ := json.Marshal(ports)
		var token store.APIToken
		s.st.DB.Where("cdn_user_id = ? AND cdn_profile_id = ?", u.ID, p.ID).First(&token)
		info.CDNChoices = append(info.CDNChoices, sub.CDNProfileChoice{ID: p.ID, Name: p.Name, Scheme: strings.ToUpper(cdnkit.Scheme(&p)), Port: cdnkit.DefaultPort(&p), Enabled: p.ExpandPerIP, PoolVersion: personalCDNVersion(u.ID, p.ID, personal, u.CDNPool.V[p.ID]), EntriesJSON: string(entriesJSON), PoolJSON: string(poolJSON), PortsJSON: string(portsJSON), ProbeSupported: p.Provider == "cloudflare", ProbePrefix: token.Prefix, ProbePort: token.CDNPort, Addresses: strings.Join(personal, "\n"), Selection: s.cdnSelectionText(&p, carrier, region, personal)})
	}
	return nil
}
func (s *Server) cdnRegionSplitOn() bool {
	var n int64
	s.st.DB.Model(&store.CDNProfile{}).Where("region_split = ?", true).Count(&n)
	return n > 0
}

// Personal lists are scoped to one account and profile. They never alter the
// shared report/pin lists, and they grant no new inbounds or network access.
func (s *Server) portalCDNPreferences(c *gin.Context) {
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
		ProfileID   string    `json:"profileId"`
		Version     string    `json:"version"`
		StalePolicy *string   `json:"stalePolicy"`
		StaleHours  *int      `json:"staleHours"`
		Addresses   *[]string `json:"addresses"`
		Disabled    *[]string `json:"disabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Addresses == nil || len(*req.Addresses) > 32 {
		badRequest(c, fmt.Errorf("请提供 addresses 数组，最多 32 个地址"))
		return
	}
	addresses, err := normalizeCDNPinnedHosts(*req.Addresses)
	if err != nil {
		badRequest(c, err)
		return
	}
	var disabled []string
	if req.Disabled != nil {
		if len(*req.Disabled) > 256 {
			badRequest(c, fmt.Errorf("最多禁用 256 个地址"))
			return
		}
		disabled, err = normalizeCDNPinnedHosts(*req.Disabled)
		if err != nil {
			badRequest(c, err)
			return
		}
	}
	profiles, err := s.cdnUserProfiles(u)
	if err != nil {
		dbError(c, err)
		return
	}
	if !slices.ContainsFunc(profiles, func(p store.CDNProfile) bool { return p.ID == req.ProfileID }) {
		c.JSON(403, gin.H{"error": "只能维护本人有权限使用的 CDN 配置"})
		return
	}
	if req.Version == "" {
		c.JSON(428, gin.H{"error": "请刷新页面获取最新版本后再保存，当前草稿可先复制保留"})
		return
	}
	if req.StalePolicy != nil && *req.StalePolicy != "keep" && *req.StalePolicy != "fallback" {
		badRequest(c, fmt.Errorf("过期策略应为 keep 或 fallback"))
		return
	}
	if req.StaleHours != nil && (*req.StaleHours < 1 || *req.StaleHours > 720) {
		badRequest(c, fmt.Errorf("过期时间为 1–720 小时"))
		return
	}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		var current store.User
		if err := tx.First(&current, "id = ?", u.ID).Error; err != nil {
			return err
		}
		if req.Version != personalCDNVersion(u.ID, req.ProfileID, current.CDNPreferred.V[req.ProfileID], current.CDNPool.V[req.ProfileID]) {
			return errPersonalCDNConflict
		}
		choices := current.CDNPreferred.V
		if choices == nil {
			choices = map[string][]string{}
		}
		if len(addresses) == 0 {
			delete(choices, req.ProfileID)
		} else {
			choices[req.ProfileID] = addresses
		}
		updates := map[string]any{"cdn_preferred": store.NewJSON(choices)}
		if req.Disabled != nil || req.StalePolicy != nil || req.StaleHours != nil {
			pools := current.CDNPool.V
			if pools == nil {
				pools = map[string]store.PersonalCDNPool{}
			}
			pool := pools[req.ProfileID]
			if req.Disabled != nil {
				pool.Disabled = disabled
			}
			if req.StalePolicy != nil {
				pool.StalePolicy = *req.StalePolicy
			}
			if req.StaleHours != nil {
				pool.StaleHours = *req.StaleHours
			}
			pools[req.ProfileID] = pool
			updates["cdn_pool"] = store.NewJSON(pools)
		}
		return tx.Model(&current).Updates(updates).Error
	})
	if err != nil {
		if errors.Is(err, errPersonalCDNConflict) {
			c.JSON(409, gin.H{"error": err.Error()})
			return
		}
		dbError(c, err)
		return
	}
	s.audit(portalActor(u, s.portalViaMail(c)), "user.cdn_preferred", u.Username, req.ProfileID, clientIP(c.Request))
	c.JSON(200, gin.H{"ok": true, "addresses": addresses})
}
