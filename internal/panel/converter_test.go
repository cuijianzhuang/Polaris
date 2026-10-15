package panel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func TestProfileConversion(t *testing.T) {
	links := "trojan://pw@hk1.example.com:443?sni=hk1.example.com#香港 01\n" +
		"vless://6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01@jp1.example.com:443?security=reality&sni=www.example.com&pbk=Z84J2IelR9ch3k8VtlVhhs5ycBUlXA7wHBWcBrjqnAw&sid=01&type=tcp#日本 01\n" +
		"trojan://pw@info.example.com:443#剩余流量：100GB\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("subscription-userinfo", "upload=1; download=2; total=1000; expire=1900000000")
		w.Write([]byte(base64.StdEncoding.EncodeToString([]byte(links))))
	}))
	defer upstream.Close()

	s := newTestServer(t)
	srv := httptest.NewServer(s.Router())
	defer srv.Close()
	s.cfg.PublicURL = srv.URL

	// SSRF guard blocks loopback upstreams by default.
	src := store.SubSource{ID: "s1", Name: "airport", Kind: "url", URL: upstream.URL, Enabled: true}
	s.st.DB.Create(&src)
	if _, err := s.fetchSource(t.Context(), &src); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("expected SSRF refusal, got %v", err)
	}
	s.st.SetSetting("allow_private_sources", "true")
	nodes, err := s.fetchSource(t.Context(), &src)
	if err != nil || len(nodes) != 3 || src.UserInfo.V.Total != 1000 {
		t.Fatalf("fetch: %v %d %+v", err, len(nodes), src.UserInfo.V)
	}
	manual := store.SubSource{ID: "s2", Name: "mine", Kind: "manual", Enabled: true,
		Content: "ss://" + base64.StdEncoding.EncodeToString([]byte("aes-128-gcm:x@9.9.9.9:8388")) + "#US 家宽"}
	s.st.DB.Create(&manual)

	p := store.SubProfile{ID: "p1", Name: "mix", Token: "profile-token-abcdefgh", Enabled: true,
		SourceIDs: store.NewJSON([]string{"s1", "s2"}),
		Operators: store.NewJSON([]sub.Operator{{Type: "filter", DropInfo: true}, {Type: "sort", RegionOrder: []string{"JP"}}, {Type: "rename", Template: "{emoji} {regionZh} {index}"}}),
		Templates: store.NewJSON(map[string]string{})}
	if err := s.st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}

	get := func(q string) (string, http.Header) {
		req, _ := http.NewRequest("GET", srv.URL+"/sub/p/profile-token-abcdefgh"+q, nil)
		req.Header.Set("User-Agent", "clash-verge/2.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
		return string(b), resp.Header
	}
	body, hdr := get("")
	for _, want := range []string{"🇯🇵 日本 01", "🇭🇰 香港 01", "🇺🇸 美国 01", `- name: "🇯🇵 日本"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "剩余流量") || strings.Index(body, "日本 01") > strings.Index(body, "香港 01") {
		t.Fatalf("chain not applied:\n%s", body)
	}
	if !strings.Contains(hdr.Get("subscription-userinfo"), "total=1000") {
		t.Fatalf("userinfo: %q", hdr.Get("subscription-userinfo"))
	}
	if b, _ := get("?format=quanx"); !strings.Contains(b, "trojan=hk1.example.com:443") {
		t.Fatalf("quanx: %s", b)
	}

	// External converter: backend receives our raw callback and fetches it.
	var gotTarget, gotCB string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTarget = r.URL.Query().Get("target")
		cb := r.URL.Query().Get("url")
		gotCB = cb
		resp, err := http.Get(cb)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		raw, _ := io.ReadAll(resp.Body)
		dec, _ := base64.StdEncoding.DecodeString(string(raw))
		w.Write([]byte(fmt.Sprintf("converted:%d", bytes.Count(dec, []byte("://")))))
	}))
	defer backend.Close()
	s.st.DB.Model(&p).Updates(map[string]any{"converter": "external", "backend": backend.URL})
	body, _ = get("")
	if gotTarget != "clash" || body != "converted:3" {
		t.Fatalf("external converter: target=%q body=%q", gotTarget, body)
	}
	// The backend gets a short-lived callback, never the permanent token.
	if strings.Contains(gotCB, "profile-token-abcdefgh") || !strings.Contains(gotCB, "/sub/cb/") {
		t.Fatalf("callback leaks the subscription token: %s", gotCB)
	}
	for i := 0; i < callbackUses; i++ {
		http.Get(gotCB)
	}
	if resp, err := http.Get(gotCB); err != nil || resp.StatusCode != 404 {
		t.Fatalf("callback should be exhausted after %d uses", callbackUses)
	}
	// Backend failure falls back to the built-in renderer.
	s.st.DB.Model(&p).Update("backend", "http://127.0.0.1:1")
	if body, _ = get(""); !strings.Contains(body, "proxies:") {
		t.Fatalf("fallback failed: %s", body)
	}

	// Preview API runs a draft.
	pr := profileReq{Name: "x", SourceIDs: []string{"s2"}}
	b, _ := json.Marshal(pr)
	req := httptest.NewRequest("POST", "/", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	c, _ := ginTestContext(rec, req)
	s.previewProfile(c)
	if !strings.Contains(rec.Body.String(), `"region":"US"`) {
		t.Fatalf("preview: %s", rec.Body.String())
	}
}

func ginTestContext(w http.ResponseWriter, r *http.Request) (*gin.Context, *gin.Engine) {
	c, e := gin.CreateTestContext(w)
	c.Request = r
	return c, e
}
