package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Exercise the delivered SSR/JS with real portal handlers and cookies. SMTP
// codes are seeded in the test-only ephemeral store; no production shortcut.
func TestLiveUserPortalBrowser(t *testing.T) {
	if os.Getenv("POLARIS_USER_PORTAL_BROWSER_TEST") == "" {
		t.Skip("POLARIS_USER_PORTAL_BROWSER_TEST required")
	}
	for _, registration := range []bool{false, true} {
		t.Run("registration="+strconv.FormatBool(registration), func(t *testing.T) {
			s, base, _ := cdnRegionFixture(t)
			s.cfg.PublicURL = base
			s.webFS = http.Dir("../../web/public")
			useMail(t, s)
			for k, v := range map[string]string{"portal_email_register": strconv.FormatBool(registration), "register_ip_daily": "0", "shop_enabled": "true", "shop_web_enabled": "true", "tickets_enabled": "true", "site_title": "Polaris", "sub_announce": "欢迎使用 Polaris。连接遇到问题，请先更新订阅并查看使用教程。"} {
				s.st.SetSetting(k, v)
			}
			s.st.DB.Model(&store.Node{}).Where("id = ?", "rn").Updates(map[string]any{"name": "香港精品线路", "status": "online", "flag": "HK", "region": "Hong Kong", "traffic_rate": 2})
			expire := time.Now().Add(30 * 24 * time.Hour)
			s.st.DB.Model(&store.User{}).Where("id = ?", "ra").Updates(map[string]any{"email": "alice.long.account.address.for.avatar@example.com", "traffic_limit": 100 << 30, "used_down": 15 << 30, "used_up": 1 << 30, "expire_at": expire})
			for _, v := range []any{
				&store.Node{ID: "tw-node", Name: "台湾备用线路-with-a-long-name", Address: "tw.example.com", Status: "offline", Flag: "TW", Region: "Taiwan"},
				&store.Inbound{ID: "tw-in", NodeID: "tw-node", Tag: "trojan", Port: 443, Core: model.CoreSingbox, Enabled: true, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoTrojan}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecTLS, ServerName: "tw.example.com"})},
				&store.GroupInbound{GroupID: "rg", InboundID: "tw-in"},
				&store.Order{ID: "my-order", UserID: "ra", PlanName: "专业版月付", Amount: "20.00", Currency: "CNY", Status: "paid", CreatedAt: time.Now()},
				&store.Order{ID: "foreign-order", UserID: "rb", PlanName: "Secret purchase", Amount: "99", Currency: "CNY", Status: "paid"},
			} {
				if err := s.st.DB.Create(v).Error; err != nil {
					t.Fatal(err)
				}
			}
			for day := range 7 {
				row := store.TrafficHourly{UserID: "ra", NodeID: "rn", Hour: time.Now().AddDate(0, 0, day-6).UTC().Truncate(time.Hour), Up: int64(day+1) * (32 << 20), Down: int64(day+1) * (200 << 20)}
				if err := s.st.DB.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			}
			var user store.User
			s.st.DB.First(&user, "id = ?", "ra")
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					http.Error(w, "method", 405)
					return
				}
				if r.URL.Path == "/session" {
					var current store.User
					if err := s.st.DB.First(&current, "id = ?", user.ID).Error; err != nil {
						http.Error(w, "user", 500)
						return
					}
					token, err := s.issuePortalMailToken(&current)
					if err != nil {
						http.Error(w, "session", 500)
						return
					}
					other, err := s.issuePortalMailToken(&current)
					if err != nil {
						http.Error(w, "session", 500)
						return
					}
					json.NewEncoder(w).Encode(map[string]string{"token": token, "otherToken": other})
					return
				}
				if r.URL.Path == "/node" {
					var input struct {
						State string `json:"state"`
					}
					if json.NewDecoder(r.Body).Decode(&input) != nil || (input.State != "online" && input.State != "offline") {
						http.Error(w, "state", 400)
						return
					}
					if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "rn").Update("status", input.State).Error; err != nil {
						http.Error(w, "update", 500)
						return
					}
					w.Write([]byte(`{"ok":true}`))
					return
				}
				http.NotFound(w, r)
			}))
			defer control.Close()

			codes := map[string]string{}
			for _, engine := range []string{"chromium", "webkit"} {
				addr := engine + "@example.com"
				purpose := "signin"
				if registration {
					purpose = "register"
				} else {
					u := newUser(&userReq{Username: engine})
					u.Email = addr
					if err := s.st.DB.Create(&u).Error; err != nil {
						t.Fatal(err)
					}
				}
				code := randomCode()
				codes[addr] = code
				if err := s.eph.put(mailKey(purpose, addr), mailCode{Hash: codeHash(addr, code), Sent: time.Now()}, mailCodeTTL); err != nil {
					t.Fatal(err)
				}
			}
			encoded, _ := json.Marshal(codes)
			cmd := exec.CommandContext(t.Context(), "python3", "../../web/scripts/user-portal-live.py")
			cmd.Env = append(os.Environ(), "POLARIS_PORTAL_TEST_URL="+base, "POLARIS_PORTAL_TEST_CONTROL="+control.URL, "POLARIS_PORTAL_TEST_CODES="+string(encoded), "POLARIS_PORTAL_REGISTRATION="+strconv.FormatBool(registration), "POLARIS_PORTAL_ADMIN_API="+s.cfg.APIBase())
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("browser: %v\n%s", err, out)
			}
			t.Log(string(out))
		})
	}
}
