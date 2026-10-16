package panel

import (
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestReminderTemplates(t *testing.T) {
	s, _, c := newAdminClient(t)
	ch := events(s)
	exp := time.Now().Add(50 * time.Hour)
	s.st.DB.Create(&store.User{ID: "u1", Username: "alice", UUID: "a", SubToken: "alice-subscription-token", Status: "active",
		TelegramID: 42, TrafficLimit: 100, UsedDown: 85, ExpireAt: &exp})
	s.st.SetSetting("tpl_traffic_warn", "Hi {user}, {percent}% used, {left} left <b>")
	s.remindUsers()
	got := []string{next(t, ch), next(t, ch)}
	all := strings.Join(got, "\n")
	if !strings.Contains(all, "Hi alice, 85% used, 15 B left &lt;b&gt;") {
		t.Fatalf("custom usage reminder (escaped): %q", got)
	}
	if !strings.Contains(all, "到期提醒") { // no custom template: built-in text
		t.Fatalf("built-in expiry reminder: %q", got)
	}
	if code, out := c.do("POST", "/telegram/template-preview", map[string]string{"text": "{user} 到期 {expire}，剩 {days} 天"}); code != 200 || !strings.Contains(out["text"].(string), "alice 到期 ") {
		t.Fatalf("preview %d %v", code, out)
	}
}
