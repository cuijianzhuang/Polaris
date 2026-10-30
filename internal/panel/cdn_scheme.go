package panel

import (
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

var cloudflareHTTPPorts = []int{80, 8080, 8880, 2052, 2082, 2086, 2095}

// Empty means HTTPS for existing profiles and old backups.
func cdnScheme(p *store.CDNProfile) string {
	if p.Scheme == "http" {
		return "http"
	}
	return "https"
}

func cdnClientPorts(p *store.CDNProfile) []int {
	if cdnScheme(p) == "http" {
		return cloudflareHTTPPorts
	}
	return cloudflareHTTPSPorts
}

func cdnDefaultPort(p *store.CDNProfile) int {
	if len(p.Ports.V) > 0 {
		return p.Ports.V[0]
	}
	if cdnScheme(p) == "http" {
		return 80
	}
	return 443
}

func cdnPortsText(p *store.CDNProfile) string {
	parts := make([]string, len(cdnClientPorts(p)))
	for i, port := range cdnClientPorts(p) {
		parts[i] = fmt.Sprint(port)
	}
	return strings.Join(parts, "、")
}

// Cloudflare's origin encryption mode affects HTTPS visitors; HTTP visitors
// reach the origin using plain HTTP even under Full / Full (strict).
func cdnOriginTLS(p *store.CDNProfile) bool {
	return cdnScheme(p) == "https" && p.TLSMode != "flexible"
}

// A scheme change cannot inherit old measurements or invalidate attached origins.
// Call under the same transaction lock as report writes and probe pairing.
type cdnSchemeProblem string

func (e cdnSchemeProblem) Error() string { return string(e) }

func validateCDNSchemeChange(tx *gorm.DB, old, next *store.CDNProfile) error {
	if cdnScheme(old) == cdnScheme(next) {
		return nil
	}
	var reports, probes int64
	if err := tx.Model(&store.CDNIPReport{}).Where("profile_id = ?", old.ID).Count(&reports).Error; err != nil {
		return err
	}
	if err := tx.Model(&store.APIToken{}).Where("cdn_profile_id = ?", old.ID).Count(&probes).Error; err != nil {
		return err
	}
	if reports > 0 || probes > 0 {
		return cdnSchemeProblem("请先移除测速点及上报结果，再切换 HTTP/HTTPS，避免复用旧协议的测速结果")
	}
	var eps []store.Endpoint
	if err := tx.Where("cdn_profile_id = ?", old.ID).Find(&eps).Error; err != nil {
		return err
	}
	for _, ep := range eps {
		var row store.Inbound
		if err := tx.First(&row, "id = ?", ep.InboundID).Error; err != nil {
			return err
		}
		m := model.Inbound{Port: row.Port, Transport: row.Transport.V, Security: row.Security.V}
		if problem := cdnProblem(&m, next, ep.Port, next.CaddyFronting); problem != "" {
			return cdnSchemeProblem(fmt.Sprintf("切换连接方式会影响入站 %s：%s", row.Tag, problem))
		}
	}
	return nil
}
