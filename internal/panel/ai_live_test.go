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

	"github.com/cuijianzhuang/polaris/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func TestLiveAIAssistantBrowser(t *testing.T) {
	if os.Getenv("POLARIS_AI_BROWSER_TEST") == "" {
		t.Skip("POLARIS_AI_BROWSER_TEST required")
	}
	s, _, _, fake, cfg := aiFixture(t, "openai")
	s.webFS = http.Dir("../../web/dist")
	srv := httptest.NewServer(s.Router())
	defer srv.Close()
	s.cfg.PublicURL = srv.URL
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("tickets_enabled", "true")
	aiNodeFixture(t, s)
	cfg.Knowledge = "用户更新订阅后选择已授权节点。AI 不能修改套餐或退款。"
	saveAIFixture(t, s, cfg)
	fake.mu.Lock()
	fake.draftOnDemand = true
	fake.answer = "请先更新订阅并核对客户端参数。REALITY 连接通过仍需确认代理认证。[K1]\n<img src=x onerror=alert('untrusted')>"
	fake.mu.Unlock()
	var serial atomic.Int64
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/fixture" {
			http.NotFound(w, r)
			return
		}
		n := serial.Add(1)
		username := strings.Repeat("a", int(n)) + "-ai-browser"
		hash, err := bcrypt.GenerateFromPassword([]byte("browser-test-password"), bcrypt.MinCost)
		if err != nil {
			http.Error(w, "fixture", 500)
			return
		}
		if err = s.st.DB.Create(&store.Admin{ID: store.NewID(), Username: username, Role: "owner", PasswordHash: string(hash)}).Error; err != nil {
			http.Error(w, "fixture", 500)
			return
		}
		u := bindingPlaceholder(t, s, "customer-"+username, username+"@example.com", 0)
		token, err := s.issuePortalMailToken(&u)
		if err != nil {
			http.Error(w, "fixture", 500)
			return
		}
		saveAIFixture(t, s, cfg)
		json.NewEncoder(w).Encode(map[string]any{"username": username, "password": "browser-test-password", "cookie": token, "nodeId": "ai-node"})
	}))
	defer control.Close()
	cmd := exec.CommandContext(t.Context(), "python3", "../../web/scripts/ai-assistant-live.py")
	cmd.Env = append(os.Environ(), "POLARIS_AI_TEST_URL="+srv.URL, "POLARIS_AI_TEST_CONTROL="+control.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, out)
	}
	t.Log(string(out))
}
