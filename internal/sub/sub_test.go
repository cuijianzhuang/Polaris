package sub

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func sample() []Node {
	return Dedupe([]Node{
		{Name: "🇯🇵 Tokyo REALITY", Server: "203.0.113.1", Port: 443, Protocol: model.ProtoVLESS, UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01",
			Flow: "xtls-rprx-vision", Transport: model.TransportSpec{Type: model.TransTCP},
			TLS: model.ClientTLS{Type: model.SecReality, ServerName: "www.example.com", PublicKey: "Z84J2IelR9ch3k8VtlVhhs5ycBUlXA7wHBWcBrjqnAw", ShortID: "0123abcd"}},
		{Name: "CDN WS", Server: "104.16.1.1", Port: 2053, Protocol: model.ProtoVLESS, UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01",
			Transport: model.TransportSpec{Type: model.TransWS, Path: "/ws", Host: "cdn.example.com"},
			TLS:       model.ClientTLS{Type: model.SecTLS, ServerName: "cdn.example.com"}},
		{Name: "XHTTP", Server: "203.0.113.1", Port: 8443, Protocol: model.ProtoVLESS, UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01",
			Transport: model.TransportSpec{Type: model.TransXHTTP, Path: "/x"}, TLS: model.ClientTLS{Type: model.SecTLS, ServerName: "a.example.com"}},
		{Name: "VMess", Server: "203.0.113.1", Port: 8080, Protocol: model.ProtoVMess, UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01",
			Transport: model.TransportSpec{Type: model.TransWS, Path: "/vm"}},
		{Name: "Trojan", Server: "203.0.113.1", Port: 8444, Protocol: model.ProtoTrojan, Password: "pw",
			TLS: model.ClientTLS{Type: model.SecTLS, ServerName: "t.example.com", Insecure: true}},
		{Name: "SS", Server: "203.0.113.1", Port: 9000, Protocol: model.ProtoShadowsocks, Method: "2022-blake3-aes-128-gcm",
			Password: "Ar9iPjxjvKbx8PmqXbX5Sg==:Bq9iPjxjvKbx8PmqXbX5Sg=="},
		{Name: "Hy2", Server: "203.0.113.1", Port: 443, Protocol: model.ProtoHysteria2, Password: "pw",
			TLS: model.ClientTLS{Type: model.SecTLS, ServerName: "h.example.com"}, ObfsPassword: "ob"},
		{Name: "TUIC", Server: "203.0.113.1", Port: 8443, Protocol: model.ProtoTUIC, UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", Password: "pw",
			TLS: model.ClientTLS{Type: model.SecTLS, ServerName: "t.example.com"}},
		{Name: "AnyTLS", Server: "203.0.113.1", Port: 2053, Protocol: model.ProtoAnyTLS, Password: "pw",
			TLS: model.ClientTLS{Type: model.SecTLS, ServerName: "a.example.com"}},
		{Name: "SS", Server: "203.0.113.2", Port: 9000, Protocol: model.ProtoShadowsocks, Method: "aes-128-gcm", Password: "legacy"},
	})
}

func TestLinks(t *testing.T) {
	r, _ := Get("links")
	b, err := r.Render(sample(), Info{}, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(string(b))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"vless://", "security=reality", "pbk=", "vmess://", "trojan://", "ss://2022-blake3-aes-128-gcm:",
		"hysteria2://", "obfs-password=ob", "tuic://", "anytls://", "#SS%202", "type=xhttp"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

func TestClash(t *testing.T) {
	r, _ := Get("clash")
	b, err := r.Render(sample(), Info{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
		Groups  []struct {
			Name    string   `yaml:"name"`
			Proxies []string `yaml:"proxies"`
		} `yaml:"proxy-groups"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("invalid yaml: %v\n%s", err, b)
	}
	if len(doc.Proxies) != 10 {
		t.Fatalf("want 10 proxies, got %d", len(doc.Proxies))
	}
	if len(doc.Groups) != 5 || len(doc.Groups[1].Proxies) != 10 || doc.Groups[4].Name != "🇯🇵 日本" {
		t.Fatalf("bad groups: %+v", doc.Groups)
	}
}

func TestSingbox(t *testing.T) {
	r, _ := Get("singbox")
	b, err := r.Render(sample(), Info{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"XHTTP"`) {
		t.Fatal("xhttp has no sing-box client and must be skipped")
	}
	if bin := os.Getenv("POLARIS_SINGBOX_BIN"); bin != "" {
		f := filepath.Join(t.TempDir(), "c.json")
		os.WriteFile(f, b, 0o600)
		if out, err := exec.Command(bin, "check", "-c", f).CombinedOutput(); err != nil {
			t.Fatalf("sing-box rejected client config: %v %s\n%s", err, out, b)
		}
	}
}

func TestSurge(t *testing.T) {
	r, _ := Get("surge")
	b, err := r.Render(sample(), Info{URL: "https://p.example/sub/x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "REALITY =") || !strings.Contains(s, "TUIC = tuic-v5") || strings.Contains(s, "Hy2 =") {
		t.Fatalf("unexpected surge output:\n%s", s)
	}
}

func TestDetect(t *testing.T) {
	cases := map[string]string{
		"clash-verge/v2.0":        "clash",
		"mihomo/1.19":             "clash",
		"SFA/1.11.0":              "singbox",
		"Surge iOS/3000":          "surge",
		"v2rayNG/1.9":             "links",
		"Shadowrocket/2000":       "links",
		"Mozilla/5.0 (Macintosh)": "html",
		"":                        "links",
	}
	for ua, want := range cases {
		if got := DetectFormat("", ua); got != want {
			t.Errorf("%q: got %s want %s", ua, got, want)
		}
	}
	if DetectFormat("clash", "v2rayNG") != "clash" {
		t.Error("query must win")
	}
}

func TestPage(t *testing.T) {
	b, err := RenderPage(Info{Title: "Polaris", URL: "https://p.example/sub/abc", Total: 100, Upload: 20, Download: 30})
	if err != nil || !strings.Contains(string(b), "data:image/png;base64,") || !strings.Contains(string(b), "width:50%") {
		t.Fatalf("bad page: %v", err)
	}
}

func TestFlag(t *testing.T) {
	if Flag("jp") != "🇯🇵" || Flag("x") != "" {
		t.Fatal("flag")
	}
}
