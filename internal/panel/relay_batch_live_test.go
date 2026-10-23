package panel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/core/gost"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestLiveRelayBatch(t *testing.T) {
	bin := os.Getenv("POLARIS_GOST_BIN")
	if bin == "" {
		t.Skip("POLARIS_GOST_BIN not set")
	}
	for _, transport := range []string{"tcp", "ws"} {
		t.Run(transport, func(t *testing.T) {
			s, c := relayBatchFixture(t)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "parallel-relay-ok") }))
			defer backend.Close()
			u, _ := url.Parse(backend.URL)
			_, p, _ := net.SplitHostPort(u.Host)
			s.st.DB.Model(&store.Node{}).Where("id IN ?", []string{"relay", "relay2", "middle", "land"}).Update("address", "127.0.0.1")
			s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Updates(map[string]any{"port": p, "listen": "127.0.0.1", "protocol": store.NewJSON(model.ProtocolSpec{Type: model.ProtoShadowsocks, Method: "aes-128-gcm"})})
			udp, e := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", p))
			if e != nil {
				t.Fatal(e)
			}
			defer udp.Close()
			go func() {
				b := make([]byte, 2048)
				for {
					n, a, e := udp.ReadFrom(b)
					if e != nil {
						return
					}
					udp.WriteTo(b[:n], a)
				}
			}()
			var preview map[string]any
			// All logical nodes run on this test host, so also avoid cross-node ports
			// that would be independent on real servers, and the test port registry.
			for tries := 0; tries < 20; tries++ {
				req := relayBatchRequestTest("tunnel", true)
				req["transport"] = transport
				preview = previewRelayBatchTest(t, c, req)
				valid := true
				for _, raw := range preview["entries"].([]any) {
					for _, raw := range raw.(map[string]any)["ports"].([]any) {
						port := int(raw.(map[string]any)["port"].(float64))
						if !coretest.Claim(port) {
							valid = false
						}
					}
				}
				if valid {
					break
				}
				preview = nil
			}
			if preview == nil {
				t.Fatal("no isolated local port set")
			}
			createRelayBatchTest(t, c, preview)
			var chains []store.Chain
			s.st.DB.Where("batch_id = ?", preview["batchId"]).Order("created_at").Find(&chains)
			if len(chains) != 2 {
				t.Fatal(chains)
			}
			start := func(spec *model.NodeSpec) *gost.Adapter {
				dir := t.TempDir()
				a := gost.New(core.Options{Version: "3.2.6", BinPath: bin, WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))})
				cfg, e := a.Render(spec)
				if e != nil {
					t.Fatal(e)
				}
				if e = a.Validate(cfg); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(dir, "gost.yaml"), cfg, 0600); e != nil {
					t.Fatal(e)
				}
				if e = a.Start(t.Context()); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { a.Stop(context.Background()) })
				return a
			}
			running := map[string]*gost.Adapter{}
			for _, id := range []string{"land", "middle", "relay", "relay2"} {
				spec, e := s.BuildSpec(id)
				if e != nil {
					t.Fatal(e)
				}
				running[id] = start(spec)
			}
			client := &http.Client{Timeout: time.Second}
			fetch := func(port int) (string, error) {
				res, e := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
				if e != nil {
					return "", e
				}
				defer res.Body.Close()
				b, e := io.ReadAll(res.Body)
				return string(b), e
			}
			for _, ch := range chains {
				port := ch.Hops.V[0].Port
				var body string
				for until := time.Now().Add(10 * time.Second); time.Now().Before(until); {
					body, e = fetch(port)
					if e == nil && body == "parallel-relay-ok" {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				if body != "parallel-relay-ok" {
					t.Fatal("entry TCP failed", ch.Hops.V[0].NodeID, body, e)
				}
				conn, e := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
				if e != nil {
					t.Fatal(e)
				}
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				conn.Write([]byte("parallel-udp"))
				b := make([]byte, 64)
				n, e := conn.Read(b)
				conn.Close()
				if e != nil || string(b[:n]) != "parallel-udp" {
					t.Fatal("entry UDP failed", ch.Hops.V[0].NodeID, e)
				}
			}
			// A correct credential from the other path still must not authenticate.
			roguePort := coretest.FreePort(t)
			start(&model.NodeSpec{Forwards: []model.ForwardRule{{Tag: "cross-path-auth", Engine: model.EngineGost, Listen: roguePort, Network: "tcp", Target: fmt.Sprintf("127.0.0.1:%d", chains[1].Hops.V[1].TunnelPort), TunnelType: transport, TunnelRole: "client", TunnelAuth: s.st.Box.MustOpen(chains[0].TunnelAuth), TunnelPath: "/polaris/" + chains[1].ID}}})
			time.Sleep(100 * time.Millisecond)
			if body, e := fetch(roguePort); e == nil && body == "parallel-relay-ok" {
				t.Fatal("one path authenticated with another path's secret")
			}
			if e = running["relay"].Stop(context.Background()); e != nil {
				t.Fatal(e)
			}
			if body, e := fetch(chains[1].Hops.V[0].Port); e != nil || body != "parallel-relay-ok" {
				t.Fatal("stopping entry one affected entry two", body, e)
			}
		})
	}
}
