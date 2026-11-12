package panel

import (
	"fmt"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/panel/cdnkit"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A scheme change cannot inherit old measurements or invalidate attached origins.
// Call under the same transaction lock as report writes and probe pairing.
type cdnSchemeProblem string

func (e cdnSchemeProblem) Error() string { return string(e) }

func validateCDNSchemeChange(tx *gorm.DB, old, next *store.CDNProfile) error {
	if cdnkit.Scheme(old) == cdnkit.Scheme(next) {
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
		if problem := cdnkit.Problem(&m, next, ep.Port, next.CaddyFronting); problem != "" {
			return cdnSchemeProblem(fmt.Sprintf("切换连接方式会影响入站 %s：%s", row.Tag, problem))
		}
	}
	return nil
}
