package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/core/gost"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func relayFixture(t *testing.T) (*Server, *apiClient) {
	t.Helper()
	s, _, c := newAdminClient(t)
	for _, id := range []string{"relay", "middle", "land"} {
		if e := s.st.DB.Create(&store.Node{ID: id, Name: id, Address: id + ".example.com", Status: "online", DesiredRev: 2, AppliedRev: 2}).Error; e != nil {
			t.Fatal(e)
		}
	}
	s.st.DB.Create(&store.Core{ID: "xray", NodeID: "land", Type: model.CoreXray, Version: "26.3.27"})
	in := store.Inbound{ID: "in", NodeID: "land", Tag: "target", Core: model.CoreXray, Port: 443, Enabled: true, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecNone})}
	if e := s.st.DB.Create(&in).Error; e != nil {
		t.Fatal(e)
	}
	return s, c
}

func previewRelayTest(t *testing.T, c *apiClient, body any) map[string]any {
	t.Helper()
	status, out := c.do("POST", "/inbounds/in/relays/preview", body)
	if status != 200 {
		t.Fatalf("preview: %d %v", status, out)
	}
	return out
}
func createRelayTest(t *testing.T, c *apiClient, preview map[string]any) string {
	t.Helper()
	code, out := c.do("POST", "/inbounds/in/relays", map[string]any{"planToken": preview["planToken"]})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	return out["id"].(string)
}
func inboundRelaysTest(t *testing.T, c *apiClient) []inboundRelayView {
	t.Helper()
	req, _ := http.NewRequest("GET", c.base+"/inbounds/in/relays", nil)
	req.Header.Set("Authorization", "Bearer "+c.tok)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	var rows []inboundRelayView
	if res.StatusCode != 200 {
		t.Fatalf("list %d", res.StatusCode)
	}
	if e = json.NewDecoder(res.Body).Decode(&rows); e != nil {
		t.Fatal(e)
	}
	return rows
}

func TestOneClickRelayLifecycle(t *testing.T) {
	for _, mode := range []string{"l4", "tunnel"} {
		t.Run(mode, func(t *testing.T) {
			s, c := relayFixture(t)
			preview := previewRelayTest(t, c, map[string]any{"mode": mode, "transport": map[string]string{"tunnel": "ws"}[mode], "relays": []map[string]any{{"nodeId": "relay"}, {"nodeId": "middle"}}})
			var n int64
			s.st.DB.Model(&store.Chain{}).Count(&n)
			if n != 0 {
				t.Fatal("preview mutated DB")
			}
			id := createRelayTest(t, c, preview)
			var ch store.Chain
			s.st.DB.First(&ch, "id = ?", id)
			if !ch.Managed || len(ch.Hops.V) != 3 || ch.Hops.V[2].InboundID != "in" {
				t.Fatalf("chain %+v", ch)
			}
			if mode == "tunnel" && !strings.HasPrefix(ch.TunnelAuth, "enc:v1:") {
				t.Fatal("tunnel secret is not sealed")
			}
			body, _ := json.Marshal(ch)
			if strings.Contains(string(body), "tunnelAuth") {
				t.Fatal("API leaks tunnel credential")
			}
			rows := inboundRelaysTest(t, c)
			if len(rows) != 1 || rows[0].State != "pending" {
				t.Fatalf("saved != applied: %+v", rows)
			}
			for _, nodeID := range []string{"relay", "middle", "land"} {
				spec, e := s.BuildSpec(nodeID)
				if e != nil {
					t.Fatal(e)
				}
				expected := 1
				if nodeID == "land" {
					expected = 0
				}
				if mode == "tunnel" {
					expected = 1
					if nodeID == "middle" {
						expected = 2
					}
					if !spec.HasCore(model.CoreGost) {
						t.Fatalf("missing GOST: %s", nodeID)
					}
				}
				if len(spec.Forwards) != expected {
					t.Fatalf("%s forwards %+v", nodeID, spec.Forwards)
				}
				for _, f := range spec.Forwards {
					if nodeID == "middle" && f.TunnelRole == "client" && f.ListenIP != "127.0.0.1" {
						t.Fatal("intermediate local hop was public")
					}
				}
			}
			u := store.User{ID: "u", Username: "alice", UUID: "11111111-1111-4111-8111-111111111111", Password: "p", SubToken: "tok", Status: "active"}
			s.st.DB.Create(&u)
			s.st.DB.Create(&store.UserAccess{UserID: u.ID, Kind: "inbound", TargetID: "in"})
			subs, e := s.subNodes(&u)
			if e != nil || len(subs) != 2 {
				t.Fatalf("direct + relay subscription: %d %v", len(subs), e)
			}
			if code, out := c.do("PUT", "/chains/"+id, map[string]any{"name": "break-managed", "mode": "l4", "hops": ch.Hops.V}); code != 409 {
				t.Fatalf("managed edit %d %v", code, out)
			}
			for nodeID, rev := range ch.TargetRevs.V {
				s.st.DB.Model(&store.Node{}).Where("id = ?", nodeID).Updates(map[string]any{"desired_rev": rev, "applied_rev": rev})
			}
			if rows := inboundRelaysTest(t, c); rows[0].State != "applied" {
				t.Fatalf("status %+v", rows)
			}
			if code, out := c.do("PUT", "/inbounds/in/relays/"+id+"/state", map[string]any{"enabled": false}); code != 200 {
				t.Fatalf("disable %d %v", code, out)
			}
			for _, nodeID := range []string{"relay", "middle", "land"} {
				spec, e := s.BuildSpec(nodeID)
				if e != nil || len(spec.Forwards) != 0 {
					t.Fatalf("disabled %s: %+v %v", nodeID, spec, e)
				}
			}
			subs, e = s.subNodes(&u)
			if e != nil || len(subs) != 1 || subs[0].Server != "land.example.com" {
				t.Fatalf("disabled subscription %+v %v", subs, e)
			}
			if code, _ := c.do("PUT", "/inbounds/in/relays/"+id+"/state", map[string]any{"enabled": true}); code != 200 {
				t.Fatal(code)
			}
			if code, out := c.do("DELETE", "/inbounds/in/relays/"+id, nil); code != 204 {
				t.Fatalf("delete %d %v", code, out)
			}
			// Replaying a consumed preview must never resurrect deleted resources.
			if code, _ := c.do("POST", "/inbounds/in/relays", map[string]any{"planToken": preview["planToken"]}); code != 201 {
				t.Fatal(code)
			}
			s.st.DB.Model(&store.Chain{}).Where("id = ?", id).Count(&n)
			if n != 0 {
				t.Fatal("deleted relay resurrected")
			}
			s.st.DB.Model(&store.Endpoint{}).Where("relay_chain_id = ?", id).Count(&n)
			if n != 0 {
				t.Fatal("relay endpoint leaked")
			}
		})
	}
}

func TestOneClickRelayValidationAndRollback(t *testing.T) {
	s, c := relayFixture(t)
	for _, body := range []any{
		map[string]any{"relays": []any{}}, map[string]any{"relays": []any{map[string]any{"nodeId": "land"}}}, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}, map[string]any{"nodeId": "relay"}}},
		map[string]any{"mode": "tunnel", "transport": "quic", "relays": []any{map[string]any{"nodeId": "relay"}}}, map[string]any{"relays": []any{map[string]any{"nodeId": "missing"}}},
	} {
		if code, out := c.do("POST", "/inbounds/in/relays/preview", body); code != 400 {
			t.Fatalf("invalid preview %d %v", code, out)
		}
	}
	req := map[string]any{"relays": []any{map[string]any{"nodeId": "relay", "port": 30123}}}
	preview := previewRelayTest(t, c, req)
	// Occupying a reviewed port must fail without silently choosing another.
	s.st.DB.Create(&store.ForwardRule{ID: "occupied", NodeID: "relay", ListenPort: 30123})
	if code, out := c.do("POST", "/inbounds/in/relays", map[string]any{"planToken": preview["planToken"]}); code != 400 {
		t.Fatalf("occupied %d %v", code, out)
	}
	s.st.DB.Delete(&store.ForwardRule{}, "id = ?", "occupied")
	raw, _ := s.st.Box.Open(preview["planToken"].(string))
	var plan relaySetupPlan
	json.Unmarshal([]byte(raw), &plan)
	wrongOwner := plan
	wrongOwner.Owner = "another-admin"
	wrongBody, _ := json.Marshal(wrongOwner)
	for _, token := range []string{raw, preview["planToken"].(string) + "tampered", s.st.Box.Seal(string(wrongBody))} {
		if code, _ := c.do("POST", "/inbounds/in/relays", map[string]any{"planToken": token}); code != 400 {
			t.Fatal("unsealed, tampered or foreign preview accepted", code)
		}
	}
	expired := plan
	expired.Expires = time.Now().Add(-time.Second)
	b, _ := json.Marshal(expired)
	if code, _ := c.do("POST", "/inbounds/in/relays", map[string]any{"planToken": s.st.Box.Seal(string(b))}); code != 400 {
		t.Fatal("expired preview accepted")
	}
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("port", 444)
	if code, _ := c.do("POST", "/inbounds/in/relays", map[string]any{"planToken": preview["planToken"]}); code != 400 {
		t.Fatal("stale preview accepted")
	}
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("port", 443)
	s.st.DB.Callback().Create().Before("gorm:create").Register("relay-fail", func(db *gorm.DB) {
		if db.Statement.Table == "endpoints" {
			db.AddError(errors.New("injected endpoint failure"))
		}
	})
	if code, _ := c.do("POST", "/inbounds/in/relays", map[string]any{"planToken": preview["planToken"]}); code != 500 {
		t.Fatal("injection did not fail", code)
	}
	s.st.DB.Callback().Create().Remove("relay-fail")
	var n int64
	for _, table := range []any{&store.Chain{}, &store.Endpoint{}, &store.IdempotencyKey{}} {
		s.st.DB.Model(table).Count(&n)
		if n != 0 {
			t.Fatalf("transaction leaked %T rows %d", table, n)
		}
	}
	createRelayTest(t, c, preview)
}

func TestOneClickRelayCoalescedState(t *testing.T) {
	s, c := relayFixture(t)
	preview := previewRelayTest(t, c, map[string]any{"mode": "tunnel", "relays": []any{map[string]any{"nodeId": "relay"}}})
	id := createRelayTest(t, c, preview)
	for _, nodeID := range []string{"relay", "land"} {
		rev, _, err := s.Publish(nodeID, "initial relay", false)
		if err != nil {
			t.Fatal(err)
		}
		s.st.DB.Model(&store.Node{}).Where("id = ?", nodeID).Update("applied_rev", rev)
	}
	for _, enabled := range []bool{false, true} {
		if code, out := c.do("PUT", "/inbounds/in/relays/"+id+"/state", map[string]any{"enabled": enabled}); code != 200 {
			t.Fatalf("state %d %v", code, out)
		}
	}
	// Both changes precede reconciliation, so Publish correctly creates no rev.
	for _, nodeID := range []string{"relay", "land"} {
		if _, changed, err := s.Publish(nodeID, "coalesced relay", false); err != nil || changed {
			t.Fatalf("unexpected revision: %v %v", changed, err)
		}
	}
	if rows := inboundRelaysTest(t, c); len(rows) != 1 || rows[0].State != "applied" {
		t.Fatalf("unchanged spec stuck pending: %+v", rows)
	}
}

func TestOneClickRelayCleanup(t *testing.T) {
	for _, target := range []string{"inbound", "node"} {
		t.Run(target, func(t *testing.T) {
			s, c := relayFixture(t)
			preview := previewRelayTest(t, c, map[string]any{"mode": "tunnel", "relays": []any{map[string]any{"nodeId": "relay"}, map[string]any{"nodeId": "middle"}}})
			id := createRelayTest(t, c, preview)
			s.st.DB.Create(&store.ForwardRule{ID: "manual", NodeID: "relay", ListenPort: 32001})
			path := "/inbounds/in"
			if target == "node" {
				path = "/nodes/middle"
			}
			if code, out := c.do("DELETE", path, nil); code != 204 {
				t.Fatalf("cleanup %d %v", code, out)
			}
			var n int64
			s.st.DB.Model(&store.Chain{}).Where("id = ?", id).Count(&n)
			if n != 0 {
				t.Fatal("managed chain leaked")
			}
			s.st.DB.Model(&store.ForwardRule{}).Where("id = ?", "manual").Count(&n)
			if n != 1 {
				t.Fatal("unrelated forward deleted")
			}
		})
	}
}

func TestOneClickRelayConcurrentReplay(t *testing.T) {
	s, c := relayFixture(t)
	cfg := *s.cfg
	cfg.DataDir = t.TempDir()
	second, e := New(&cfg, s.log, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { db, _ := second.st.DB.DB(); db.Close() }()
	server := httptest.NewServer(second.Router())
	defer server.Close()
	preview := previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}})
	body, _ := json.Marshal(map[string]any{"planToken": preview["planToken"]})
	bases := []string{c.base, server.URL + s.cfg.APIBase()}
	results := make([]commandReply, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = callUserCommand(bases[i], c.tok, "/inbounds/in/relays", string(body), fmt.Sprintf("request-%d", i))
		}(i)
	}
	close(start)
	wg.Wait()
	for _, r := range results {
		if r.err != nil || r.status != 201 {
			t.Fatalf("concurrent %+v", r)
		}
	}
	var n int64
	s.st.DB.Model(&store.Chain{}).Count(&n)
	if n != 1 {
		t.Fatal("duplicate chains", n)
	}
	if !results[0].replay && !results[1].replay {
		t.Fatal("no replay")
	}
	// A new transport key replaying the same plan also gets a finished cache row.
	r := callUserCommand(c.base, c.tok, "/inbounds/in/relays", string(body), "third-key")
	if r.status != 201 || !r.replay {
		t.Fatalf("third key %+v", r)
	}
	r = callUserCommand(c.base, c.tok, "/inbounds/in/relays", string(body), "third-key")
	if r.status != 201 || !r.replay {
		t.Fatalf("third key second replay %+v", r)
	}
}

func TestLiveOneClickRelay(t *testing.T) {
	bin := os.Getenv("POLARIS_GOST_BIN")
	if bin == "" {
		t.Skip("POLARIS_GOST_BIN not set")
	}
	for _, transport := range []string{"tcp", "ws"} {
		t.Run(transport, func(t *testing.T) {
			s, c := relayFixture(t)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "one-click-through-two-hops") }))
			defer backend.Close()
			u, _ := url.Parse(backend.URL)
			_, p, _ := net.SplitHostPort(u.Host)
			s.st.DB.Model(&store.Node{}).Where("id IN ?", []string{"relay", "middle", "land"}).Update("address", "127.0.0.1")
			s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Updates(map[string]any{"port": p, "listen": "127.0.0.1", "protocol": store.NewJSON(model.ProtocolSpec{Type: model.ProtoShadowsocks, Method: "aes-128-gcm"})})
			udp, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", p))
			if err != nil {
				t.Fatal(err)
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
			preview := previewRelayTest(t, c, map[string]any{"mode": "tunnel", "transport": transport, "relays": []any{map[string]any{"nodeId": "relay"}, map[string]any{"nodeId": "middle"}}})
			id := createRelayTest(t, c, preview)
			isolateLocalRelayPorts(t, s, id)
			var ch store.Chain
			s.st.DB.First(&ch, "id = ?", id)
			for _, id := range []string{"land", "middle", "relay"} {
				spec, e := s.BuildSpec(id)
				if e != nil {
					t.Fatal(e)
				}
				dir := t.TempDir()
				a := gost.New(core.Options{Version: "3.2.6", BinPath: bin, WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))})
				cfg, e := a.Render(spec)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(dir, "gost.yaml"), cfg, 0600); e != nil {
					t.Fatal(e)
				}
				if e = a.Start(t.Context()); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { a.Stop(context.Background()) })
			}
			var got string
			client := &http.Client{Timeout: 2 * time.Second}
			for deadline := time.Now().Add(12 * time.Second); time.Now().Before(deadline); {
				res, e := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", ch.Hops.V[0].Port))
				if e == nil {
					b, _ := io.ReadAll(res.Body)
					res.Body.Close()
					got = string(b)
					if got == "one-click-through-two-hops" {
						break
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
			if got != "one-click-through-two-hops" {
				t.Fatalf("tunnel forwarding failed: %q", got)
			}
			conn, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", ch.Hops.V[0].Port), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(4 * time.Second))
			payload := []byte("udp-through-authenticated-relay")
			if _, err = conn.Write(payload); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 128)
			n, err := conn.Read(buf)
			if err != nil || string(buf[:n]) != string(payload) {
				t.Fatalf("UDP tunnel: %q %v", buf[:n], err)
			}
			// The receiver must not accept an unauthenticated/wrong-password client.
			badPort := coretest.FreePort(t)
			dir := t.TempDir()
			rogue := gost.New(core.Options{Version: "3.2.6", BinPath: bin, WorkDir: dir})
			cfg, err := rogue.Render(&model.NodeSpec{Forwards: []model.ForwardRule{{Tag: "wrong-password", Engine: model.EngineGost, Listen: badPort, Network: "tcp", Target: fmt.Sprintf("127.0.0.1:%d", ch.Hops.V[len(ch.Hops.V)-1].TunnelPort), TunnelType: transport, TunnelRole: "client", TunnelAuth: "wrong-password", TunnelPath: "/polaris/" + ch.ID}}})
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "gost.yaml"), cfg, 0600); err != nil {
				t.Fatal(err)
			}
			if err = rogue.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { rogue.Stop(context.Background()) })
			time.Sleep(150 * time.Millisecond)
			if res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", badPort)); err == nil {
				b, _ := io.ReadAll(res.Body)
				res.Body.Close()
				if string(b) == "one-click-through-two-hops" {
					t.Fatal("wrong tunnel password was accepted")
				}
			}

		})
	}
}
