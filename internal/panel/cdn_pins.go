package panel

import (
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func normalizeCDNPinnedHosts(input []string) ([]string, error) {
	hosts := []string{}
	for _, raw := range input {
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
		if host == "" {
			continue
		}
		// Keep the existing pinnedHosts API/storage contract, now also accepting
		// public IP literals. IPv4-mapped IPv6 and bracketed IPv6 normalize to
		// the same connection address used by speed-test reports.
		ipText := host
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			ipText = host[1 : len(host)-1]
		}
		if ip, err := netip.ParseAddr(ipText); err == nil {
			ip = ip.Unmap()
			if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Zone() != "" {
				return nil, fmt.Errorf("手动优选地址 %q 不是公网 IP", raw)
			}
			host = ip.String()
		} else if len(host) > 253 || !cdnHostRe.MatchString(host) || strings.HasPrefix(host, "*.") {
			return nil, fmt.Errorf("手动优选地址 %q 无效，请填写公网 IP 或完整域名，不带协议、端口、路径或通配符", raw)
		}
		if !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	return hosts, nil
}

// Remove the previous manual portion, then append the explicit replacement.
// Automatic addresses stay automatic unless the admin explicitly pins them.
func setCDNPinnedHosts(p *store.CDNProfile, hosts, oldPinned []string) {
	list := slices.DeleteFunc(slices.Clone(p.PreferredIPs.V), func(a string) bool { return slices.Contains(oldPinned, a) })
	p.PinnedHosts = store.NewJSON(hosts)
	p.PreferredIPs = store.NewJSON(nonNilStrs(withPinned(list, hosts)))
}

// This narrow update never submits an old copy of the speed-test results or
// other profile settings from the browser.
func (s *Server) updateCDNPinnedHosts(c *gin.Context) {
	var req struct {
		Hosts *[]string `json:"pinnedHosts"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Hosts == nil {
		badRequest(c, fmt.Errorf("请提供 pinnedHosts 数组，清空地址请传 []"))
		return
	}
	hosts, err := normalizeCDNPinnedHosts(*req.Hosts)
	if err != nil {
		badRequest(c, err)
		return
	}
	var p store.CDNProfile
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		if err := tx.First(&p, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		setCDNPinnedHosts(&p, hosts, p.PinnedHosts.V)
		return tx.Model(&p).Updates(map[string]any{"pinned_hosts": p.PinnedHosts, "preferred_ips": p.PreferredIPs}).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "cdn.update_pinned_hosts", p.Name, fmt.Sprintf("%d addresses", len(hosts)))
	c.JSON(http.StatusOK, p)
}
