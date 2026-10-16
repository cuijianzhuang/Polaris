package panel

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Reminder templates: the wording users receive on Telegram for the usage
// and expiry reminders and the paused / expired / resumed notices can be
// replaced (settings tpl_<kind>), with {variables} filled per user. Custom
// text is sent as plain text; an empty template keeps the built-in one.

// reminderVars are the values a template can use for u.
func (s *Server) reminderVars(u *store.User) map[string]string {
	used, total := u.Used(), u.TrafficLimit
	v := map[string]string{
		"user": u.Username, "used": sub.HumanBytes(used), "total": "不限", "left": "不限", "percent": "0",
		"expire": "永久", "days": "—", "reset": "", "site": s.st.GetSetting("site_title", "Polaris"),
		"sub_url": s.subURL(u.SubToken), "support": s.st.GetSetting("sub_support_url", ""),
	}
	if total > 0 {
		v["total"], v["left"] = sub.HumanBytes(total), sub.HumanBytes(max(total-used, 0))
		v["percent"] = fmt.Sprintf("%.0f", float64(used)*100/float64(total))
	}
	if u.ExpireAt != nil {
		v["expire"] = u.ExpireAt.Local().Format("2006-01-02 15:04")
		v["days"] = fmt.Sprintf("%d", int(time.Until(*u.ExpireAt).Hours()/24))
	}
	var i sub.Info
	planInfo(&i, u)
	v["reset"] = i.ResetNote
	return v
}

func renderTemplate(tpl string, vars map[string]string) string {
	for k, val := range vars {
		tpl = strings.ReplaceAll(tpl, "{"+k+"}", val)
	}
	return tpl
}

// customReminder renders the custom template for kind as Telegram HTML
// (the text escaped), or "" when none is set.
func (s *Server) customReminder(kind string, u *store.User) string {
	tpl := strings.TrimSpace(s.st.GetSetting("tpl_"+kind, ""))
	if tpl == "" || u == nil {
		return ""
	}
	return html.EscapeString(renderTemplate(tpl, s.reminderVars(u)))
}

// previewReminder: POST /telegram/template-preview {kind, text} — renders a
// template with a sample (or the first bound) user.
func (s *Server) previewReminder(c *gin.Context) {
	var req struct {
		Text string `json:"text"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var u store.User
	if s.st.DB.Where("is_service = ?", false).Order("telegram_id DESC, created_at").First(&u).Error != nil {
		exp := time.Now().AddDate(0, 0, 3)
		u = store.User{Username: "alice", TrafficLimit: 100 << 30, UsedDown: 82 << 30, ExpireAt: &exp, SubToken: "example-subscription-token"}
	}
	c.JSON(http.StatusOK, gin.H{"text": renderTemplate(req.Text, s.reminderVars(&u)), "user": u.Username})
}
