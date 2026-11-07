package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Real portal handlers, cookies and bot callbacks against a local Telegram API.
// The /test/answer control exists only in this test's fake server.
func TestLivePortalTelegramBrowser(t *testing.T) {
	if os.Getenv("POLARIS_PORTAL_BROWSER_TEST") == "" {
		t.Skip("POLARIS_PORTAL_BROWSER_TEST required")
	}
	for _, mail := range []bool{false, true} {
		t.Run("mail="+strconv.FormatBool(mail), func(t *testing.T) {
			s := newTestServer(t)
			s.webFS = http.Dir("../../web/public")
			s.st.SetSetting("portal_enabled", "true")
			if mail {
				useMail(t, s)
				s.st.SetSetting("portal_email_register", "true")
			}
			u := store.User{ID: "telegram-user", Username: "alice", UUID: "test-uuid", Password: "test-password", SubToken: "telegram-user-subscription-token", TelegramID: 701, Status: "active"}
			if err := s.st.DB.Create(&u).Error; err != nil {
				t.Fatal(err)
			}
			fake := &fakeTG{}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/test/answer" {
					var input struct {
						ID, Pick string
						Chat     int64
					}
					if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&input) != nil {
						http.Error(w, "invalid", 400)
						return
					}
					if input.Pick == "" {
						fake.push(msg(input.Chat, "/start "+portalLoginPrefix+input.ID))
					} else {
						fake.push(cb(input.Chat, "ul:"+input.ID+":"+input.Pick))
					}
					w.Write([]byte(`{"ok":true}`))
					return
				}
				fake.ServeHTTP(w, r)
			}))
			defer api.Close()
			for k, v := range map[string]string{"tg_enabled": "true", "tg_token": s.st.Box.Seal("123:abc"), "tg_api_base": api.URL, "tg_poll_timeout": "0"} {
				s.st.SetSetting(k, v)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go s.tg.run(ctx)
			waitUntil(t, "bot running", s.tgWebLoginEnabled)
			srv := httptest.NewServer(s.Router())
			defer srv.Close()
			cmd := exec.CommandContext(t.Context(), "python3", "../../web/scripts/portal-telegram-live.py")
			cmd.Env = append(os.Environ(), "POLARIS_PORTAL_TEST_URL="+srv.URL, "POLARIS_PORTAL_EXPECT_MAIL="+strconv.FormatBool(mail), "POLARIS_TG_TEST_CONTROL="+api.URL, "POLARIS_PORTAL_ADMIN_API="+s.cfg.APIBase())
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("browser: %v\n%s", err, out)
			} else {
				t.Log(string(out))
			}
			if !strings.Contains(fake.textsTo(9999), "还没有绑定账号") {
				t.Fatal("unbound account was not guided to binding")
			}
		})
	}
}
