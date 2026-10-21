package panel

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/xray"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestVisionUDP443Subscription(t *testing.T) {
	s, srv, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "203.0.113.9"})
	s.st.DB.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"})
	body := map[string]any{"nodeId": "n1", "core": "xray", "tag": "vision", "port": 2443,
		"protocol":  map[string]any{"type": "vless", "flow": "xtls-rprx-vision"},
		"transport": map[string]any{"type": "tcp"},
		"security":  map[string]any{"type": "reality", "realityDest": "www.example.com:443"}}
	if code, out := c.do("POST", "/inbounds", body); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create inbound: %d %v", code, out)
	}
	var row store.Inbound
	if err := s.st.DB.First(&row, "tag = ?", "vision").Error; err != nil {
		t.Fatal(err)
	}
	code, user := c.do("POST", "/users", map[string]any{"username": "alice"})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create user: %d %v", code, user)
	}
	fetch := func(format string) string {
		t.Helper()
		resp, err := http.Get(srv.URL + "/sub/" + user["subToken"].(string) + "?format=" + format)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("subscription %s: status=%d err=%v body=%s", format, resp.StatusCode, err, b)
		}
		if format == "links" {
			b, err = base64.StdEncoding.DecodeString(string(b))
			if err != nil {
				t.Fatal(err)
			}
		}
		return string(b)
	}
	for _, enabled := range []bool{false, true, false} {
		body["protocol"].(map[string]any)["visionUDP443"] = enabled
		if code, out := c.do("PUT", "/inbounds/"+row.ID, body); code != http.StatusOK {
			t.Fatalf("update inbound: %d %v", code, out)
		}
		if err := s.st.DB.First(&row, "id = ?", row.ID).Error; err != nil {
			t.Fatal(err)
		}
		if row.Protocol.V.VisionUDP443 != enabled || row.Protocol.V.Flow != "xtls-rprx-vision" {
			t.Fatalf("persisted protocol: %+v", row.Protocol.V)
		}
		links := fetch("links")
		if !strings.Contains(links, "flow=xtls-rprx-vision") || strings.Contains(links, "flow=xtls-rprx-vision-udp443") != enabled {
			t.Fatalf("enabled=%v: unexpected share links: %s", enabled, links)
		}
		for _, format := range []string{"singbox", "clash"} {
			if text := fetch(format); strings.Contains(text, "-udp443") || !strings.Contains(text, "xtls-rprx-vision") {
				t.Fatalf("incompatible %s subscription: %s", format, text)
			}
		}
		spec, err := s.BuildSpec("n1")
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := xray.New(core.Options{Version: "26.3.27"}).Render(spec)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(cfg), "-udp443") || strings.Contains(string(cfg), "visionUDP443") || !strings.Contains(string(cfg), `"flow": "xtls-rprx-vision"`) {
			t.Fatalf("subscription preference leaked into server config: %s", cfg)
		}
	}
}
