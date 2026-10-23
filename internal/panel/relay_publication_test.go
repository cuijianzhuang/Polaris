package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func relayTestUser(t *testing.T, s *Server) store.User {
	t.Helper()
	u := store.User{ID: "relay-user", Username: "relay-user", UUID: "11111111-1111-4111-8111-111111111111", Password: "p", SubToken: "relay-user-subscription-token", Status: "active"}
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.UserAccess{UserID: u.ID, Kind: "inbound", TargetID: "in"}).Error; err != nil {
		t.Fatal(err)
	}
	return u
}

func TestRelayPublicationLifecycle(t *testing.T) {
	s, c := relayFixture(t)
	u := relayTestUser(t, s)
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	assertEntries := func(want int, host string) {
		t.Helper()
		nodes, err := s.subNodes(&u)
		if err != nil || len(nodes) != want {
			t.Fatalf("nodes %d, want %d: %v", len(nodes), want, err)
		}
		for _, n := range nodes {
			if host != "" && n.Server != host {
				t.Fatalf("unexpected published host %s", n.Server)
			}
		}
		code, out := c.do("GET", "/users/"+u.ID+"/links", nil)
		if code != 200 {
			t.Fatal(code, out)
		}
		if want == 0 && out["links"] != nil {
			t.Fatal("share links remain", out)
		}
		if host == "relay.example.com" {
			for _, v := range out["links"].([]any) {
				if strings.Contains(v.(map[string]any)["uri"].(string), "land.example.com") {
					t.Fatal("share exposes a direct endpoint", out)
				}
			}
		}
	}
	assertEntries(2, "")
	if code, out := c.do("PUT", "/inbounds/in/publication", map[string]any{"relayOnly": true}); code != 200 || out["relayOnly"] != true {
		t.Fatal(code, out)
	}
	assertEntries(1, "relay.example.com")
	s.st.DB.Model(&store.Node{}).Where("id = ?", "relay").Update("status", "offline")
	assertEntries(1, "relay.example.com") // do not switch to direct on failure
	s.st.DB.Model(&store.Node{}).Where("id = ?", "relay").Update("status", "online")
	if code, _ := c.do("PUT", "/inbounds/in/relays/"+id+"/state", map[string]any{"enabled": false}); code != 200 {
		t.Fatal(code)
	}
	assertEntries(0, "")
	if code, _ := c.do("DELETE", "/inbounds/in/relays/"+id, nil); code != 204 {
		t.Fatal(code)
	}
	assertEntries(0, "")
	// Neither an empty endpoint set nor a new relay creates a direct escape.
	s.st.DB.Where("inbound_id = ?", "in").Delete(&store.Endpoint{})
	assertEntries(0, "")
	createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	var n int64
	s.st.DB.Model(&store.Endpoint{}).Where("inbound_id = ? AND kind = ?", "in", "direct").Count(&n)
	if n != 0 {
		t.Fatal("relay-only creation inserted direct endpoint")
	}
	assertEntries(1, "relay.example.com")
	// A saved direct endpoint is dormant, not deleted by the policy.
	s.st.DB.Create(&store.Endpoint{ID: "direct", InboundID: "in", Kind: "direct"})
	if code, _ := c.do("PUT", "/inbounds/in/publication", map[string]any{"relayOnly": false}); code != 200 {
		t.Fatal(code)
	}
	assertEntries(2, "")
}

func TestRelayPublicationCopiesAndHealth(t *testing.T) {
	s, c := relayFixture(t)
	if code, _ := c.do("PUT", "/inbounds/in/publication", map[string]any{"relayOnly": true}); code != 200 {
		t.Fatal(code)
	}
	if len(s.probeTargets("")) != 0 {
		t.Fatal("scheduled direct probe must skip relay-only inbound")
	}
	s.st.DB.Create(&store.InboundHealth{InboundID: "in", OK: false, Error: "old direct failure"})
	if code, out := c.do("GET", "/inbounds/in", nil); code != 200 || out["health"] != nil {
		t.Fatal("old direct health shown for relay-only publication", code, out)
	}
	if code, _ := c.do("POST", "/inbounds/in/probe", nil); code != 400 {
		t.Fatal("direct probe must explain relay-only policy", code)
	}
	code, exported := c.do("GET", "/inbounds/in/export", nil)
	if code != 200 || exported["relayOnly"] != true {
		t.Fatal("export loses policy", code, exported)
	}
	delete(exported, "relayOnly") // old editor knows nothing about this field
	if code, out := c.do("PUT", "/inbounds/in", exported); code != 200 || out["inbound"].(map[string]any)["relayOnly"] != true {
		t.Fatal("old editor resets policy", code, out)
	}
	if code, _ := c.do("POST", "/inbounds/in/clone", map[string]any{}); code != 200 {
		t.Fatal("clone", code)
	}
	var clone store.Inbound
	if err := s.st.DB.First(&clone, "node_id = ? AND id <> ?", "land", "in").Error; err != nil || !clone.RelayOnly {
		t.Fatal("clone loses publication policy", clone, err)
	}
	exported["tag"], exported["port"], exported["relayOnly"] = "imported", 31995, true
	if code, out := c.do("POST", "/inbounds", exported); code != 201 || out["inbound"].(map[string]any)["relayOnly"] != true {
		t.Fatal("import loses policy", code, out)
	}
	if code, _ := c.do("PUT", "/inbounds/in/publication", map[string]any{}); code != 400 {
		t.Fatal("missing boolean accepted", code)
	}
	if code, _ := c.do("PUT", "/inbounds/missing/publication", map[string]any{"relayOnly": true}); code != 404 {
		t.Fatal("missing inbound", code)
	}
}

func TestRelayPublicationFiltersCDNAndForeignChain(t *testing.T) {
	s, c := relayFixture(t)
	u := relayTestUser(t, s)
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Updates(map[string]any{"relay_only": true, "transport": store.NewJSON(model.TransportSpec{Type: model.TransWS}), "security": store.NewJSON(model.SecuritySpec{Type: model.SecTLS, ServerName: "tls.example.com"})})
	s.st.DB.Create(&store.CDNProfile{ID: "cdn", Name: "cdn", OriginDomain: "cdn.example.com"})
	s.st.DB.Create(&store.Endpoint{ID: "cdn-entry", InboundID: "in", Kind: "cdn", CDNProfileID: "cdn"})
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	nodes, err := s.subNodes(&u)
	if err != nil || len(nodes) != 1 || nodes[0].Server != "relay.example.com" {
		t.Fatal("CDN/direct leaked", nodes, err)
	}
	var chain store.Chain
	s.st.DB.First(&chain, "id = ?", id)
	chain.Hops.V[len(chain.Hops.V)-1].InboundID = "another-inbound"
	s.st.DB.Save(&chain)
	if code, _ := c.do("POST", "/inbounds/in/endpoints", map[string]any{"kind": "relay", "relayChainId": id}); code != 400 {
		t.Fatal("foreign chain endpoint accepted", code)
	}
	if nodes, err = s.subNodes(&u); err != nil || len(nodes) != 0 {
		t.Fatal("foreign chain accepted as this inbound's relay", nodes, err)
	}
}
