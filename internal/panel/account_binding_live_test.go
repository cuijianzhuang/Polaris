package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestLiveAccountBindingBrowser(t *testing.T) {
	if os.Getenv("POLARIS_USER_PORTAL_BROWSER_TEST") == "" {
		t.Skip("POLARIS_USER_PORTAL_BROWSER_TEST required")
	}
	s, srv, _ := newAdminClient(t)
	s.cfg.PublicURL = srv.URL
	s.webFS = http.Dir("../../web/public")
	mail := useMail(t, s)
	useBindingBot(t, s)
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("portal_email_register", "false")
	s.mailCodeLimit = &sharedLimiter{s: s.shared, name: "binding-browser-mail", perMinute: 600000}
	var serial atomic.Int64
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method", 405)
			return
		}
		var body struct {
			Mode, Email, ID, Pick string
			Chat                  int64
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "json", 400)
			return
		}
		if r.URL.Path == "/answer" {
			s.tg.portalTelegramBindPrompt(body.Chat, body.ID)
			s.tg.onCallback("binding-live", body.Chat, 1, "ub:"+body.ID+":"+body.Pick, tgUser{ID: body.Chat})
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		if r.URL.Path == "/code" {
			messages := mail.To(body.Email)
			if len(messages) == 0 {
				http.Error(w, "code not sent", 404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"code": sixDigits.FindString(messages[len(messages)-1].Subject)})
			return
		}
		if r.URL.Path != "/fixture" {
			http.NotFound(w, r)
			return
		}
		id := store.NewID()
		index := serial.Add(1)
		registered := bindingPlaceholder(t, s, "registered-"+id, id+"@example.com", 0)
		subscriber := bindingSubscriber(t, s, "subscriber-"+id)
		subscriber.TelegramID = 1000 + index
		if err := s.st.DB.Model(&subscriber).Update("telegram_id", subscriber.TelegramID).Error; err != nil {
			http.Error(w, "fixture", 500)
			return
		}
		current, viaMail := registered, true
		if body.Mode == "email" {
			current, viaMail = subscriber, false
		}
		if body.Mode == "conflict" {
			s.st.DB.Model(&registered).Updates(map[string]any{"plan_id": "existing-plan", "status": "active"})
		}
		token, err := s.issuePortalTokenVia(&current, viaMail)
		if err != nil {
			http.Error(w, "session", 500)
			return
		}
		code := "123456"
		if err = s.eph.put(mailKey("signin", registered.Email), mailCode{Hash: codeHash(registered.Email, code), Sent: time.Now()}, mailCodeTTL); err != nil {
			http.Error(w, "code", 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"chat": subscriber.TelegramID, "cookie": token, "email": registered.Email, "loginCode": code, "subscription": s.subURL(subscriber.SubToken), "subToken": subscriber.SubToken, "oldToken": current.SubToken, "username": subscriber.Username, "currentUsername": current.Username})
	}))
	defer control.Close()
	cmd := exec.CommandContext(t.Context(), "python3", "../../web/scripts/account-binding-live.py")
	cmd.Env = append(os.Environ(), "POLARIS_PORTAL_TEST_URL="+srv.URL, "POLARIS_PORTAL_TEST_CONTROL="+control.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, out)
	}
	t.Log(strings.TrimSpace(string(out)))
}
