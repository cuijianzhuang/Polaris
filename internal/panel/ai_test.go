package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

type fakeAI struct {
	mu             sync.Mutex
	requests       []map[string]any
	headers        []http.Header
	answer, format string
	status         int
	redirect       string
	models         int
	draftOnDemand  bool
}

func (f *fakeAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = append(f.headers, r.Header.Clone())
	if f.redirect != "" {
		http.Redirect(w, r, f.redirect, http.StatusTemporaryRedirect)
		return
	}
	if r.Method == http.MethodGet {
		f.models++
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "older-chat", "created": 1}, map[string]any{"id": "latest-chat", "display_name": "Newest chat model", "created": 2}}})
		return
	}
	var req map[string]any
	json.NewDecoder(r.Body).Decode(&req)
	f.requests = append(f.requests, req)
	answer := f.answer
	if f.draftOnDemand {
		body, _ := json.Marshal(req)
		if strings.Contains(string(body), "任务：draft") {
			answer = `{"core":"xray","protocol":"vless","transport":"tcp","security":"reality","port":443,"serverName":"www.microsoft.com"}`
		}
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		w.Write([]byte(`{"error":"private-provider-secret"}`))
		return
	}
	if f.format == "anthropic" {
		json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "text", "text": answer}}, "stop_reason": "end_turn"})
	} else {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": answer}, "finish_reason": "stop"}}})
	}
}
func (f *fakeAI) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return ""
	}
	b, _ := json.Marshal(f.requests[len(f.requests)-1])
	return string(b)
}
func aiFixture(t *testing.T, format string) (*Server, *httptest.Server, *apiClient, *fakeAI, aiConfig) {
	t.Helper()
	s, srv, c := newAdminClient(t)
	fake := &fakeAI{format: format, answer: "依据当前检测结果，先核对客户端参数，再检查认证与传输。[K1]"}
	provider := httptest.NewServer(fake)
	t.Cleanup(provider.Close)
	s.st.SetSetting("allow_private_sources", "true")
	cfg := aiConfig{Enabled: true, PortalEnabled: true, Provider: "custom", BaseURL: provider.URL, Format: format, Model: "test-chat", APIKey: s.st.Box.Seal("test-provider-key"), DailyBudget: 300, PortalDailyLimit: 20, MaxOutputTokens: 4096, AllowPrivate: true}
	saveAIFixture(t, s, cfg)
	return s, srv, c, fake, cfg
}
func saveAIFixture(t *testing.T, s *Server, cfg aiConfig) {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.st.SetSetting(aiConfigKey, string(b)); err != nil {
		t.Fatal(err)
	}
}
func aiNodeFixture(t *testing.T, s *Server) {
	t.Helper()
	now := time.Now()
	for _, v := range []any{&store.Node{ID: "ai-node", Name: "Test node", Address: "203.0.113.38", Status: "online", LastSeen: &now, LastError: "token=node-secret"}, &store.Core{ID: "ai-core", NodeID: "ai-node", Type: model.CoreXray, Version: "26.3.27", Status: "running"}} {
		if err := s.st.DB.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestAIProviderFormatsAndLiveModels(t *testing.T) {
	for _, format := range []string{"openai", "anthropic"} {
		t.Run(format, func(t *testing.T) {
			s, _, c, f, cfg := aiFixture(t, format)
			if code, out := c.do("POST", "/ai/test", nil); code != 200 {
				t.Fatal(code, out)
			}
			f.mu.Lock()
			header := f.headers[0]
			body := f.requests[0]
			f.mu.Unlock()
			if format == "anthropic" {
				if header.Get("x-api-key") != "test-provider-key" || header.Get("anthropic-version") == "" || body["system"] == nil || header.Get("Authorization") != "" {
					t.Fatal("incorrect Claude wire format")
				}
			} else if header.Get("Authorization") != "Bearer test-provider-key" {
				t.Fatal("OpenAI auth missing")
			}
			cfg.APIKey = ""
			code, out := c.do("POST", "/ai/models", map[string]any{"config": cfg})
			if code != 200 {
				t.Fatal(code, out)
			}
			models := out["models"].([]any)
			if models[0].(map[string]any)["id"] != "latest-chat" {
				t.Fatal("model discovery ordering", out)
			}
			if code, out = c.do("GET", "/ai/settings", nil); code != 200 || strings.Contains(fmt.Sprint(out), "test-provider-key") || out["keySet"] != true {
				t.Fatal("settings exposed key", code)
			}
			var stored store.Setting
			s.st.DB.First(&stored, "key = ?", aiConfigKey)
			if strings.Contains(stored.Value, "test-provider-key") {
				t.Fatal("provider key stored in plaintext")
			}
			if code, out = c.do("GET", "/settings", nil); code != 200 || strings.Contains(fmt.Sprint(out), aiConfigKey) {
				t.Fatal("generic settings exposed AI config")
			}
		})
	}
}

func TestAIConfigurationConflictAndSecretDestination(t *testing.T) {
	s, _, c, _, cfg := aiFixture(t, "openai")
	_, initial := c.do("GET", "/ai/settings", nil)
	rev := initial["revision"]
	cfg.APIKey = ""
	cfg.Knowledge = "用户 FAQ：请先更新订阅。"
	if code, out := c.do("PUT", "/ai/settings", map[string]any{"config": cfg, "revision": rev}); code != 200 {
		t.Fatal(code, out)
	}
	if code, _ := c.do("PUT", "/ai/settings", map[string]any{"config": cfg, "revision": rev}); code != 409 {
		t.Fatal("stale editor overwrote settings", code)
	}
	_, now := c.do("GET", "/ai/settings", nil)
	cfg.BaseURL += "/foreign"
	if code, _ := c.do("PUT", "/ai/settings", map[string]any{"config": cfg, "revision": now["revision"]}); code != 400 {
		t.Fatal("secret reused on new destination", code)
	}
	if code, _ := c.do("POST", "/ai/models", map[string]any{"config": cfg}); code != 400 {
		t.Fatal("discovery reused secret on new destination", code)
	}
	// Settings reads bypass the one-minute settings cache across replicas.
	stored, _ := s.aiConfig()
	stored.Enabled = false
	saveAIFixture(t, s, stored)
	if s.aiPortalOn() {
		t.Fatal("disabled feature remained cached")
	}
}

func TestAIInboundDraftValidationAndReadOnlyBoundary(t *testing.T) {
	s, _, c, f, _ := aiFixture(t, "openai")
	aiNodeFixture(t, s)
	f.answer = `{"core":"xray","protocol":"vless","transport":"tcp","security":"reality","port":443,"serverName":"www.microsoft.com"}`
	req := map[string]any{"mode": "draft", "nodeId": "ai-node", "prompt": "创建 REALITY 入站"}
	code, out := c.do("POST", "/ai/ask", req)
	if code != 200 {
		t.Fatal(code, out)
	}
	plan := out["draft"].(map[string]any)
	if plan["protocol"] != "vless" || plan["port"] != float64(443) {
		t.Fatal(out)
	}
	var count int64
	s.st.DB.Model(&store.Inbound{}).Count(&count)
	if count != 0 {
		t.Fatal("model created an inbound")
	}
	var n store.Node
	s.st.DB.First(&n, "id = ?", "ai-node")
	if n.DesiredRev != 0 {
		t.Fatal("model published configuration")
	}
	for _, secret := range []string{"203.0.113.38", "node-secret", "test-provider-key"} {
		if strings.Contains(f.last(), secret) {
			t.Fatal("outbound AI context leaked a secret", secret)
		}
	}
	f.answer = `{"core":"xray","protocol":"invented-protocol","transport":"tcp","security":"none","port":443}`
	if code, _ = c.do("POST", "/ai/ask", req); code != 422 {
		t.Fatal("invented protocol accepted", code)
	}
	f.answer = `{"core":"xray","protocol":"vless","transport":"tcp","security":"reality","port":443,"serverName":"www.microsoft.com","extra":{"execute":"command"}}`
	if code, _ = c.do("POST", "/ai/ask", req); code != 422 {
		t.Fatal("unvalidated Extra accepted", code)
	}
	f.answer = `{"core":"xray","protocol":"vless","transport":"tcp","security":"reality","port":443,"serverName":"www.microsoft.com"}`
	s.st.DB.Create(&store.Inbound{ID: "occupied", NodeID: "ai-node", Tag: "occupied", Port: 443, Core: model.CoreXray, Enabled: true, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecNone})})
	if code, _ = c.do("POST", "/ai/ask", req); code != 422 {
		t.Fatal("conflicting port accepted", code)
	}
}

func TestAIPortalIsolationCSRFQuotaAndKnowledge(t *testing.T) {
	s, srv, _, f, cfg := aiFixture(t, "openai")
	s.st.SetSetting("portal_enabled", "true")
	cfg.PortalDailyLimit = 1
	cfg.DailyBudget = 2
	saveAIFixture(t, s, cfg)
	u := bindingPlaceholder(t, s, "ai-user", "self@example.com", 0)
	other := bindingSubscriber(t, s, "other-paid-user")
	s.st.DB.Model(&other).Update("email", "other-private@example.com")
	browser := bindingBrowser(t, s, srv.URL, u, true)
	if code, _ := browser.req("POST", "/u/assistant/chat", map[string]any{"prompt": "我的订阅如何更新？"}, false); code != 403 {
		t.Fatal("portal bypassed CSRF", code)
	}
	if code, _ := browser.req("POST", "/u/assistant/chat", map[string]any{"prompt": "查看别人", "nodeId": "foreign"}, true); code != 400 {
		t.Fatal("portal accepted foreign context selector", code)
	}
	code, body := browser.req("POST", "/u/assistant/chat", map[string]any{"prompt": "我的订阅如何更新？ password=secret-pass vless://secret-uuid@host:443 192.168.5.8"}, true)
	if code != 200 {
		t.Fatal(code, body)
	}
	for _, secret := range []string{u.Email, u.Password, u.SubToken, other.Email, other.SubToken, "secret-pass", "secret-uuid", "192.168.5.8"} {
		if secret != "" && strings.Contains(f.last(), secret) {
			t.Fatal("customer context leaked private data", secret)
		}
	}
	var answer aiAnswer
	json.Unmarshal([]byte(body), &answer)
	if len(answer.Sources) == 0 || answer.TicketDraft == nil {
		t.Fatal("missing knowledge citations or human handoff draft")
	}
	for _, source := range answer.Sources {
		if source.Document != "docs/user-guide.md" {
			t.Fatal("portal received admin-only manuals", source.Document)
		}
	}
	if code, _ = browser.req("POST", "/u/assistant/chat", map[string]any{"prompt": "再问一次"}, true); code != 429 {
		t.Fatal("portal daily budget not enforced", code)
	}
	otherBrowser := bindingBrowser(t, s, srv.URL, other, true)
	if code, _ = otherBrowser.req("POST", "/u/assistant/chat", map[string]any{"prompt": "我的订阅如何更新？"}, true); code != 200 {
		t.Fatal("an exhausted account consumed another account's global budget", code)
	}
	cfg.PortalEnabled = false
	saveAIFixture(t, s, cfg)
	if code, _ = browser.req("POST", "/u/assistant/chat", map[string]any{"prompt": "继续"}, true); code != 503 {
		t.Fatal("disabled portal stayed usable", code)
	}
}

func TestAIProviderErrorsDoNotLeakOrFollowRedirects(t *testing.T) {
	_, _, c, f, _ := aiFixture(t, "openai")
	f.status = 401
	if code, out := c.do("POST", "/ai/test", nil); code != 502 || strings.Contains(fmt.Sprint(out), "private-provider-secret") {
		t.Fatal(code, out)
	}
	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++; w.Write([]byte("unexpected")) }))
	defer target.Close()
	f.redirect = target.URL
	if code, _ := c.do("POST", "/ai/test", nil); code != 502 {
		t.Fatal(code)
	}
	if targetCalls != 0 {
		t.Fatal("AI credentials forwarded to redirect")
	}
}

func TestAIKnowledgeScopeAndRedaction(t *testing.T) {
	sources := aiKnowledgeFor("REALITY 入站 端口 TLS 证书", "", false)
	if len(sources) == 0 {
		t.Fatal("project knowledge index empty")
	}
	public := aiKnowledgeFor("管理员 授权服务器 SSH 密钥 用户中心 订阅", "", true)
	for _, s := range public {
		if s.Document != "docs/user-guide.md" {
			t.Fatal("admin knowledge exposed")
		}
	}
	input := "password=short-secret token=short-token test@example.com 203.0.113.2 2606:4700::1111 12345678-1234-1234-1234-123456789012 vless://abc@host:443"
	output := aiRedact(input)
	for _, secret := range []string{"short-secret", "short-token", "test@example.com", "203.0.113.2", "2606:4700::1111", "12345678-1234", "vless://"} {
		if strings.Contains(output, secret) {
			t.Fatal("redaction missed", secret)
		}
	}
}

func TestAIPermissionsAndPrivateDestinationPolicy(t *testing.T) {
	s, srv, root, f, cfg := aiFixture(t, "openai")
	// Reading the saved provider's model list needs no surprise sudo prompt.
	_, login := root.do("POST", "/auth/login", map[string]string{"username": "root", "password": "old-password"})
	reader := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: login["token"].(string)}
	cfg.APIKey = ""
	if code, out := reader.do("POST", "/ai/models", map[string]any{"config": cfg}); code != 200 {
		t.Fatal("saved discovery required sudo", code, out)
	}
	if code, _ := reader.do("PUT", "/ai/settings", map[string]any{}); code != 403 {
		t.Fatal("settings bypassed sudo", code)
	}
	cfg.BaseURL += "/other"
	if code, out := reader.do("POST", "/ai/models", map[string]any{"config": cfg}); code != 403 || out["sudoRequired"] != true {
		t.Fatal("transient destination bypassed sudo", code, out)
	}
	s.st.DB.Model(&store.Admin{}).Where("id = ?", "a1").Update("role", "viewer")
	if code, _ := root.do("POST", "/ai/test", nil); code != 401 && code != 403 {
		t.Fatal("restricted role retained AI access", code)
	}
	// Private opt-in is independent of subscription source settings.
	stored, _ := s.aiConfig()
	stored.AllowPrivate = false
	before := len(f.last())
	if _, err := s.askAI(context.Background(), stored, "test", []aiMessage{{Role: "user", Content: "test"}}); err == nil {
		t.Fatal("private provider reachable without opt-in")
	}
	if len(f.last()) != before {
		t.Fatal("private policy sent a provider request")
	}
}

func TestAICancellationReleasesProviderSlot(t *testing.T) {
	s, _, _, _, cfg := aiFixture(t, "openai")
	started := make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer provider.Close()
	cfg.BaseURL = provider.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.askAI(ctx, cfg, "test", []aiMessage{{Role: "user", Content: "test"}}); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request cancellation ignored")
	}
	if len(s.aiSlots) != 0 {
		t.Fatal("cancelled request leaked a concurrency slot")
	}
}

func TestAIReadOnlyPolicyKeepsConfigurationWritesBlocked(t *testing.T) {
	s, _, c, _, cfg := aiFixture(t, "openai")
	policyLicense(t, s, false)
	expirePolicyGrace(t, s)
	if code, out := c.do("POST", "/ai/test", nil); code != 200 {
		t.Fatal("read-only AI unavailable", code, out)
	}
	_, state := c.do("GET", "/ai/settings", nil)
	cfg.APIKey = ""
	if code, _ := c.do("PUT", "/ai/settings", map[string]any{"config": cfg, "revision": state["revision"]}); code != http.StatusPaymentRequired {
		t.Fatal("AI settings bypassed management write freeze", code)
	}
}

func TestAIDraftTargetNameValidation(t *testing.T) {
	for _, name := range []string{"", "127.0.0.1", "localhost", "example..com", "bad\t.example.com"} {
		if aiDomainName(name) {
			t.Fatal("invalid REALITY hostname", name)
		}
	}
	for _, name := range []string{"www.microsoft.com", "example.xn--p1ai"} {
		if !aiDomainName(name) {
			t.Fatal("valid hostname rejected", name)
		}
	}
}
