package panel

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func useBindingBot(t *testing.T, s *Server) *fakeTG {
	t.Helper()
	fake := &fakeTG{}
	api := httptest.NewServer(fake)
	t.Cleanup(api.Close)
	s.st.SetSetting("tg_enabled", "true")
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:binding-test"))
	s.st.SetSetting("tg_api_base", api.URL)
	s.tg.mu.Lock()
	s.tg.me, s.tg.running = "polaris_bot", true
	s.tg.token, s.tg.base = "123:binding-test", api.URL
	s.tg.mu.Unlock()
	return fake
}

func TestPortalTelegramBindingProvesBothAccounts(t *testing.T) {
	for _, scenario := range []string{"fresh", "existing-paid", "current-paid", "both-paid", "identity-conflict", "wrong-number", "cancelled", "wrong-session", "wrong-secret", "bot-disabled", "revoked-session"} {
		t.Run(scenario, func(t *testing.T) {
			s, srv, _ := newAdminClient(t)
			s.st.SetSetting("portal_enabled", "true")
			s.st.SetSetting("portal_email_register", "false")
			fake := useBindingBot(t, s)
			current := bindingPlaceholder(t, s, "registered", "registered@example.com", 0)
			if scenario == "current-paid" || scenario == "both-paid" {
				s.st.DB.Model(&current).Updates(map[string]any{"status": "active", "plan_id": "paid-current", "traffic_limit": int64(1 << 30)})
			}
			var actor store.User
			if scenario != "fresh" {
				if scenario == "current-paid" {
					actor = bindingPlaceholder(t, s, "telegram-only", "", 707)
				} else {
					actor = bindingSubscriber(t, s, "telegram-subscriber")
					actor.TelegramID = 707
					s.st.DB.Model(&actor).Update("telegram_id", 707)
				}
				if scenario == "identity-conflict" {
					s.st.DB.Model(&actor).Update("email", "another@example.com")
				}
			}
			browser := bindingBrowser(t, s, srv.URL, current, true)
			start := browser.jsonW("POST", "/u/telegram/bind/start", nil)
			id, secret := start["id"].(string), start["secret"].(string)
			if !strings.Contains(start["url"].(string), "ubind_"+id) {
				t.Fatal(start)
			}
			input := map[string]any{"id": id, "secret": secret}
			if st, _ := browser.req("POST", "/u/telegram/bind/poll", input, false); st != 403 {
				t.Fatal("binding bypassed CSRF", st)
			}
			if out := browser.jsonW("POST", "/u/poll", input); out["status"] != "expired" {
				t.Fatal("binding approved a sign-in", out)
			}
			if scenario == "cancelled" {
				browser.jsonW("POST", "/u/telegram/bind/cancel", input)
			}
			s.tg.portalTelegramBindPrompt(707, id)
			if scenario != "cancelled" && !strings.Contains(fake.textsTo(707), "已验证邮箱：registered@example.com") {
				t.Fatal("binding prompt did not identify the verified account", fake.textsTo(707))
			}
			pick := fmt.Sprint(start["code"])
			if scenario == "wrong-number" {
				pick = "00"
			}
			s.tg.onCallback("test", 707, 1, "ub:"+id+":"+pick, tgUser{ID: 707})
			if scenario == "wrong-session" {
				other := bindingBrowser(t, s, srv.URL, current, true)
				if out := other.jsonW("POST", "/u/telegram/bind/poll", input); out["status"] != "expired" {
					t.Fatal("another browser claimed a binding", out)
				}
			}
			if scenario == "wrong-secret" {
				if out := browser.jsonW("POST", "/u/telegram/bind/poll", map[string]any{"id": id, "secret": "wrong"}); out["status"] != "expired" {
					t.Fatal(out)
				}
			}
			if scenario == "bot-disabled" {
				s.st.SetSetting("tg_enabled", "false")
			}
			if scenario == "revoked-session" {
				s.st.DB.Model(&store.PortalSession{}).Where("user_id = ?", current.ID).Update("revoked", true)
			}
			code, body := browser.req("POST", "/u/telegram/bind/poll", input, true)
			blocked := scenario == "both-paid" || scenario == "identity-conflict" || scenario == "wrong-number" || scenario == "cancelled" || scenario == "bot-disabled" || scenario == "revoked-session"
			switch scenario {
			case "both-paid", "identity-conflict":
				if code != 409 {
					t.Fatal(code, body)
				}
			case "wrong-number":
				if code != 200 || !strings.Contains(body, `"status":"denied"`) {
					t.Fatal(code, body)
				}
			case "cancelled":
				if code != 200 || !strings.Contains(body, `"status":"expired"`) {
					t.Fatal(code, body)
				}
			case "bot-disabled":
				if code != 503 {
					t.Fatal(code, body)
				}
			case "revoked-session":
				if code != 401 {
					t.Fatal(code, body)
				}
			default:
				if code != 200 || !strings.Contains(body, `"status":"ok"`) {
					t.Fatal(code, body)
				}
			}
			var got store.User
			if blocked {
				s.st.DB.First(&got, "id = ?", current.ID)
				if got.TelegramID != 0 || got.Email != current.Email {
					t.Fatal("failed challenge changed identity")
				}
				return
			}
			canonical := current
			if actor.ID != "" && scenario != "current-paid" {
				canonical = actor
			}
			s.st.DB.First(&got, "id = ?", canonical.ID)
			if got.Email != current.Email || got.TelegramID != 707 {
				t.Fatal("verified identities did not meet on the same account")
			}
			assertBindingUnchanged(t, s, canonical)
			if code, body := browser.req("GET", "/u", nil, false); code != 200 || !strings.Contains(body, canonical.SubToken) {
				t.Fatal("binding did not renew browser session", code)
			}
			if out := browser.jsonW("POST", "/u/telegram/bind/poll", input); out["status"] != "expired" {
				t.Fatal("binding replay accepted", out)
			}
		})
	}
}
