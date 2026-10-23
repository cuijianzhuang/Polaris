package panel

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/core/gost"
	"github.com/cuijianzhuang/polaris/internal/core/xray"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/miekg/dns"
)

func TestLiveRelayBusinessThroughTunnel(t *testing.T) {
	xbin, gbin := os.Getenv("POLARIS_XRAY_BIN"), os.Getenv("POLARIS_GOST_BIN")
	if xbin == "" || gbin == "" {
		t.Skip("POLARIS_XRAY_BIN and POLARIS_GOST_BIN required")
	}
	s, c := relayFixture(t)
	u := relayTestUser(t, s)
	s.st.DB.Model(&store.Node{}).Where("id IN ?", []string{"relay", "middle", "land"}).Update("address", "127.0.0.1")
	s.st.DB.Model(&store.Node{}).Where("id = ?", "land").Update("agent_features", store.NewJSON([]string{model.RelayBusinessFeature}))
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Updates(map[string]any{"listen": "127.0.0.1", "port": coretest.FreePort(t)})
	var preview map[string]any
	for tries := 0; tries < 20; tries++ {
		preview = previewRelayTest(t, c, map[string]any{"mode": "tunnel", "transport": "ws", "relays": []any{map[string]any{"nodeId": "relay"}, map[string]any{"nodeId": "middle"}}})
		valid := true
		for _, raw := range preview["ports"].([]any) {
			if !coretest.Claim(int(raw.(map[string]any)["port"].(float64))) {
				valid = false
			}
		}
		if valid {
			break
		}
		preview = nil
	}
	if preview == nil {
		t.Fatal("no isolated ports")
	}
	id := createRelayTest(t, c, preview)
	markRelayApplied(t, s, id)
	specs := map[string]*model.NodeSpec{}
	running := map[string]*gost.Adapter{}
	dirs := map[string]string{}
	for _, node := range []string{"land", "middle", "relay"} {
		spec, err := s.BuildSpec(node)
		if err != nil {
			t.Fatal(err)
		}
		specs[node] = spec
		dir := t.TempDir()
		dirs[node] = dir
		a := gost.New(core.Options{Version: "3.2.6", BinPath: gbin, WorkDir: dir, APIListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(coretest.FreePort(t)))})
		cfg, err := a.Render(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err = core.AtomicWrite(filepath.Join(dir, "gost.yaml"), cfg, 0600); err != nil {
			t.Fatal(err)
		}
		if err = a.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		defer a.Stop(context.Background())
		running[node] = a
	}
	nativeDir := t.TempDir()
	xa := xray.New(core.Options{Version: "26.3.27", BinPath: xbin, WorkDir: nativeDir, APIListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(coretest.FreePort(t))), BindToParent: true})
	raw, err := xa.Render(specs["land"])
	if err != nil {
		t.Fatal(err)
	}
	var native map[string]any
	json.Unmarshal(raw, &native)
	for _, v := range native["outbounds"].([]any) {
		o := v.(map[string]any)
		if o["protocol"] == "freedom" {
			o["settings"] = map[string]any{"ipsBlocked": []string{"224.0.0.0/4"}, "finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.0/8"}}}}
		}
	}
	cfg, _ := json.Marshal(native)
	if err = core.AtomicWrite(filepath.Join(nativeDir, "xray.json"), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err = xa.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer xa.Stop(context.Background())
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "via-real-ws-tunnel") }))
	defer backend.Close()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ds := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
	})}
	go ds.ActivateAndServe()
	defer ds.Shutdown()
	defer fakeBusinessRunner(t, s, func(req model.RelayBusinessRequest) model.RelayBusinessResult {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		result, _ := xa.RunRelayDiagnostic(ctx, req)
		return result
	})()
	path := "/inbounds/in/relays/" + id + "/diagnose/business"
	body := map[string]any{"tcpUrl": backend.URL, "udpAddress": pc.LocalAddr().String()}
	if code, out := c.do("POST", path, body); code != 200 || out["businessVerified"] != true || out["cleanupVerified"] != true {
		t.Fatal("real tunnel not verified", code, out)
	}
	specs["relay"].Forwards[0].TunnelAuth = "wrong-tunnel-credential"
	broken, err := running["relay"].Render(specs["relay"])
	if err != nil {
		t.Fatal(err)
	}
	if err = core.AtomicWrite(filepath.Join(dirs["relay"], "gost.yaml"), broken, 0600); err != nil {
		t.Fatal(err)
	}
	if err = running["relay"].Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if code, out := c.do("POST", path, body); code != 200 || out["businessVerified"] != false || out["cleanupVerified"] != true {
		t.Fatal("wrong tunnel password accepted", code, out)
	}
	var saved store.User
	s.st.DB.First(&saved, "id = ?", u.ID)
	if saved.UsedUp != 0 || saved.UsedDown != 0 {
		t.Fatal("real user billed")
	}
}
