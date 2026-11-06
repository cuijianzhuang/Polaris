package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestLiveAnyTLSProxyChainToXrayReality(t *testing.T) {
	bin, xbin := os.Getenv("POLARIS_SINGBOX_BIN"), os.Getenv("POLARIS_XRAY_BIN")
	if bin == "" || xbin == "" {
		t.Skip("POLARIS_SINGBOX_BIN and POLARIS_XRAY_BIN required")
	}
	s, c := relayFixture(t)
	u := relayTestUser(t, s)
	s.st.DB.Where("user_id = ?", u.ID).Delete(&store.UserAccess{})
	if err := s.st.DB.Create(&store.UserAccess{UserID: u.ID, Kind: "inbound", TargetID: "proxy-in"}).Error; err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.Node{}).Where("id IN ?", []string{"relay", "land"}).Update("address", "127.0.0.1")
	tls := coretest.TLS(t, "127.0.0.1")
	if err := s.st.DB.Create(&store.Certificate{ID: "entry-cert", Provider: "manual", Cert: tls.CertPEM, Key: s.st.Box.Seal(tls.KeyPEM)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.Core{ID: "entry-core", NodeID: "relay", Type: model.CoreSingbox, Version: "1.13.5"}).Error; err != nil {
		t.Fatal(err)
	}
	entry := store.Inbound{ID: "proxy-in", NodeID: "relay", Tag: "BWG-HK-anytls", Core: model.CoreSingbox, Enabled: true, Port: coretest.FreePort(t), Listen: "127.0.0.1", Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoAnyTLS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecTLS, CertificateID: "entry-cert"})}
	if err := s.st.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dest.EnableHTTP2 = true
	dest.StartTLS()
	defer dest.Close()
	sec := coretest.Reality(t)
	sec.RealityDest = strings.TrimPrefix(dest.URL, "https://")
	sec.RealityPrivateKey = s.st.Box.Seal(sec.RealityPrivateKey)
	if err := s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Updates(map[string]any{"listen": "127.0.0.1", "port": coretest.FreePort(t), "security": store.NewJSON(sec), "protocol": store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"})}).Error; err != nil {
		t.Fatal(err)
	}
	if code, out := c.do("POST", "/chains", chainReq{Name: "hk-to-tw", Mode: "proxy", Hops: []store.ChainHop{{NodeID: "relay", InboundID: entry.ID}, {NodeID: "land", InboundID: "in"}}}); code != 201 {
		t.Fatal(code, out)
	}
	for _, instance := range []struct {
		node               string
		typ                model.CoreType
		bin, version, file string
	}{{"land", model.CoreXray, xbin, "26.3.27", "xray.json"}, {"relay", model.CoreSingbox, bin, "1.13.5", "singbox.json"}} {
		spec, err := s.BuildSpec(instance.node)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		a, err := core.New(instance.typ, core.Options{BinPath: instance.bin, Version: instance.version, WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := a.Render(spec)
		if err != nil {
			t.Fatal(err)
		}
		if instance.typ == model.CoreXray {
			// This isolated native test intentionally uses a loopback backend.
			var cfg map[string]any
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			for _, item := range cfg["outbounds"].([]any) {
				o := item.(map[string]any)
				if o["protocol"] == "freedom" {
					o["settings"] = map[string]any{"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.0/8"}}}}
				}
			}
			raw, _ = json.Marshal(cfg)
		}
		if err := a.Validate(raw); err != nil {
			t.Fatal(err)
		}
		if err := core.AtomicWrite(filepath.Join(dir, instance.file), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		defer a.Stop(context.Background())
	}
	nodes, err := s.subNodes(&u)
	if err != nil || len(nodes) != 1 || nodes[0].Protocol != model.ProtoAnyTLS || !strings.Contains(nodes[0].Name, "hk-to-tw") {
		t.Fatal("proxy chain subscription must use the authorized entry protocol", nodes, err)
	}
	relaySubscriptionTransfer(t, bin, nodes[0], tls.CertPEM)
}
