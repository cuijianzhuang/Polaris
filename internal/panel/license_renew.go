package panel

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/license"
)

// Renewal: the license server's daily check says where the admin can renew
// (its Telegram shop bot or website shop); the License page shows those as
// buttons, and the admin is told 7 days, 1 day and on the day before the
// license expires, and once when it has expired.

type licenseRenew struct {
	URL string `json:"url,omitempty"` // website shop (a new purchase)
	Bot string `json:"bot,omitempty"` // Telegram shop bot (renews this license)
}

// cleanRenew keeps only HTTPS links; the bot link must point at t.me.
func cleanRenew(shop, bot string) licenseRenew {
	ok := func(raw, host string) bool {
		u, err := url.Parse(raw)
		return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && len(raw) <= 300 && (host == "" || u.Host == host)
	}
	var r licenseRenew
	if ok(shop, "") {
		r.URL = shop
	}
	if ok(bot, "t.me") {
		r.Bot = bot
	}
	return r
}

func (s *Server) licenseRenewLinks() licenseRenew {
	var r licenseRenew
	_ = json.Unmarshal([]byte(s.st.GetSetting("license_renew", "")), &r)
	return r
}

func (s *Server) saveLicenseRenew(r licenseRenew) error {
	b, _ := json.Marshal(r)
	return s.st.SetSetting("license_renew", string(b))
}

// expiryTier is 7, 1 or 0 days before expiry, -1 once expired; 99 = no notice.
func expiryTier(l *license.License, now time.Time) (int, int) {
	t, err := time.ParseInLocation("2006-01-02", l.ExpiresAt, time.UTC)
	if l.ExpiresAt == "" || err != nil {
		return 99, 0
	}
	left := int(t.Add(24*time.Hour).Sub(now).Hours() / 24)
	switch {
	case l.Expired(now):
		return -1, 0
	case left < 1:
		return 0, 0
	case left < 2:
		return 1, 1
	case left <= 7:
		return 7, left
	}
	return 99, left
}

// remindLicenseExpiry notifies the admin once per tier and license term.
func (s *Server) remindLicenseExpiry() {
	l := s.installedLicense()
	if l == nil {
		return
	}
	tier, left := expiryTier(l, time.Now())
	if tier == 99 {
		return
	}
	// "<expiry>:<tier>": a renewal changes the expiry and starts over.
	var lastExp string
	lastTier := 100
	if v := s.st.GetSetting("license_expiry_notice", ""); v != "" {
		if exp, t, ok := strings.Cut(v, ":"); ok {
			lastExp = exp
			fmt.Sscan(t, &lastTier)
		}
	}
	if lastExp == l.ExpiresAt && tier >= lastTier {
		return
	}
	if err := s.st.SetSetting("license_expiry_notice", fmt.Sprintf("%s:%d", l.ExpiresAt, tier)); err != nil {
		s.log.Error("license expiry notice", "err", err)
		return
	}
	var text string
	switch tier {
	case -1:
		text = fmt.Sprintf("⚠️ 许可证 %s 已于 %s 到期，面板已恢复为社区版（已生效的配置保持不变）。", l.ID, l.ExpiresAt)
	case 0:
		text = fmt.Sprintf("⏰ 许可证 %s 今天（%s）到期。", l.ID, l.ExpiresAt)
	default:
		text = fmt.Sprintf("⏰ 许可证 %s 将于 %s 到期，还有 %d 天。", l.ID, l.ExpiresAt, left)
	}
	r := s.licenseRenewLinks()
	switch {
	case r.Bot != "":
		text += "\n续费：" + r.Bot
	case r.URL != "":
		text += "\n购买：" + r.URL
	default:
		text += "\n请联系授权提供方续费。"
	}
	s.notify(Event{Kind: "license", Text: text})
}
