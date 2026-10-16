package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestInstallScriptPinsAgentChecksum(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.Router())
	defer srv.Close()
	bin := []byte("agent-binary")
	os.MkdirAll(filepath.Join(s.cfg.DataDir, "downloads"), 0o755)
	os.WriteFile(filepath.Join(s.cfg.DataDir, "downloads", "polaris-agent-linux-amd64"), bin, 0o755)
	sum := sha256.Sum256(bin)

	resp, err := http.Get(srv.URL + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	script := string(b)
	for _, want := range []string{`SHA_amd64="` + hex.EncodeToString(sum[:]) + `"`, `SHA_arm64=""`, `PANEL_VERSION="dev"`, "SHA256 校验失败",
		`PANEL="https://127.0.0.1:`, `CA="` + s.ca.Fingerprint() + `"`, `PANEL_HTTP="` + srv.URL + `"`} {
		if !strings.Contains(script, want) {
			t.Fatalf("install.sh missing %q", want)
		}
	}
	if strings.Contains(script, "__") {
		t.Fatal("unreplaced placeholder in install.sh")
	}
}

func TestInstallCommandUsesRequestHost(t *testing.T) {
	s, _, c := newAdminClient(t)
	code, out := c.do("POST", "/nodes", map[string]any{"name": "tokyo"})
	if code != 201 {
		t.Fatalf("create node %d %v", code, out)
	}
	cmd := out["command"].(string)
	host := strings.TrimPrefix(strings.TrimSuffix(c.base, s.cfg.APIBase()), "http://")
	tok := out["token"].(string)
	if want := "curl -fsSL http://" + host + "/j/" + tok + " | bash"; cmd != want {
		t.Fatalf("command %q, want %q", cmd, want)
	}
	// The one-click link serves install.sh with the token inside; unknown
	// tokens get a script that only explains.
	get := func(path string) (int, string) {
		resp, err := http.Get("http://" + host + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, body := get("/j/" + tok); st != 200 || !strings.Contains(body, `TOKEN="`+tok+`"`) || strings.Contains(body, "__PANEL") {
		t.Fatalf("join script %d:\n%.300s", st, body)
	}
	if st, body := get("/j/NotARealToken12345"); st != 404 || !strings.Contains(body, "exit 1") || strings.Contains(body, "polaris-agent") {
		t.Fatalf("unknown token %d: %s", st, body)
	}
	if _, body := get("/install.sh"); !strings.Contains(body, `TOKEN="" DATA=""`) {
		t.Fatal("plain install.sh must not carry a token")
	}
	// Without POLARIS_AGENT_HOSTS / PUBLIC_URL the agent endpoint follows the
	// host the admin used, never 127.0.0.1.
	r := httptest.NewRequest("GET", "/install.sh", nil)
	r.Host = "203.0.113.7:8080"
	if got := s.agentURLFor(r); got != "https://203.0.113.7:9443" && !strings.HasPrefix(got, "https://203.0.113.7:") {
		t.Fatalf("agent URL %s", got)
	}
	s.cfg.AgentHosts = []string{"panel.example.com"}
	if got := s.agentURLFor(r); !strings.HasPrefix(got, "https://panel.example.com:") {
		t.Fatalf("configured agent host ignored: %s", got)
	}
}

func TestLocalNodeToken(t *testing.T) {
	s := newTestServer(t)
	s.cfg.PublicURL = "https://panel.example.com"
	tok, created, err := LocalNodeToken(s.cfg, "")
	if err != nil || !created || len(tok) < 20 {
		t.Fatalf("%v %v %q", err, created, tok)
	}
	tok2, created, err := LocalNodeToken(s.cfg, "")
	if err != nil || created || tok2 == tok {
		t.Fatalf("second call should reuse the node with a new token: %v %v", err, created)
	}
	var n store.Node
	s.st.DB.First(&n, "name = ?", "本机")
	if n.Address != "panel.example.com" || n.TokenHash != hashToken(tok2) {
		t.Fatalf("node: %+v", n)
	}
}

func TestAgentCacheVersionMarker(t *testing.T) {
	s := newTestServer(t)
	dir := s.downloadsDir()
	os.MkdirAll(dir, 0o755)
	if s.agentCacheCurrent() {
		t.Fatal("empty cache reported current")
	}
	for _, a := range agentArches {
		os.WriteFile(filepath.Join(dir, "polaris-agent-linux-"+a), []byte("x"), 0o755)
	}
	if !s.agentCacheCurrent() {
		t.Fatal("hand-placed binaries (no marker) should be trusted")
	}
	os.WriteFile(filepath.Join(dir, ".version"), []byte("v0.0.1\n"), 0o644)
	if s.agentCacheCurrent() {
		t.Fatal("binaries of another version reported current")
	}
}

func TestAgentBinaryNeedsNodeCert(t *testing.T) {
	s := newTestServer(t)
	os.MkdirAll(s.downloadsDir(), 0o755)
	os.WriteFile(filepath.Join(s.downloadsDir(), "polaris-agent-linux-amd64"), []byte("bin"), 0o755)
	w := httptest.NewRecorder()
	s.serveAgentBinary(w, httptest.NewRequest("GET", "/agent/binary/amd64", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("download without a node certificate: %d", w.Code)
	}
}

func TestRenewDue(t *testing.T) {
	now := time.Now()
	mk := func(life, left time.Duration) *store.Certificate {
		c, _, err := pki.SelfSigned([]string{"x"}, life)
		if err != nil {
			t.Fatal(err)
		}
		exp := now.Add(left)
		return &store.Certificate{Cert: string(c), ExpireAt: &exp}
	}
	cases := []struct {
		life, left time.Duration
		want       bool
	}{
		{90 * 24 * time.Hour, 40 * 24 * time.Hour, false},
		{90 * 24 * time.Hour, 20 * 24 * time.Hour, true},
		{6 * 24 * time.Hour, 5 * 24 * time.Hour, false}, // 6-day IP certs: not every day
		{6 * 24 * time.Hour, 36 * time.Hour, true},
	}
	for _, c := range cases {
		if got := renewDue(mk(c.life, c.left), now); got != c.want {
			t.Errorf("life %s, left %s: got %v", c.life, c.left, got)
		}
	}
}
