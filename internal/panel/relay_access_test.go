package panel

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"connectrpc.com/connect"
	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func relayAccessFixture(t *testing.T) (*Server, *apiClient, string) {
	t.Helper()
	s, c := relayFixture(t)
	for id, ip := range map[string]string{"relay": "198.51.100.1", "middle": "198.51.100.2", "land": "198.51.100.3"} {
		if e := s.st.DB.Model(&store.Node{}).Where("id = ?", id).Updates(map[string]any{"public_ipv4": ip, "agent_features": store.NewJSON([]string{model.RelayAccessFeature})}).Error; e != nil {
			t.Fatal(e)
		}
	}
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}, map[string]any{"nodeId": "middle"}}}))
	return s, c, id
}

func TestRelayAccessLifecycle(t *testing.T) {
	s, c, id := relayAccessFixture(t)
	if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true, "proxyProtocol": 2}); code != 200 {
		t.Fatal(code, out)
	}
	rows := inboundRelaysTest(t, c)
	for _, n := range rows[0].Nodes {
		if n.NodeID == "land" && n.RequiredRev <= 2 {
			t.Fatal("landing source guard did not wait for a new revision", n)
		}
	}
	for _, nodeID := range []string{"relay", "middle", "land"} {
		spec, e := s.BuildSpec(nodeID)
		if e != nil {
			t.Fatal(e)
		}
		switch nodeID {
		case "relay":
			if len(spec.AccessRules) != 0 || len(spec.Forwards) != 1 || spec.Forwards[0].SendProxy != 2 || spec.Forwards[0].AcceptProxy {
				t.Fatalf("entry %+v", spec)
			}
		case "middle":
			if len(spec.AccessRules) != 1 || spec.AccessRules[0].Sources[0] != "198.51.100.1/32" || !spec.Forwards[0].AcceptProxy || spec.Forwards[0].SendProxy != 2 {
				t.Fatalf("middle %+v", spec)
			}
		case "land":
			if len(spec.AccessRules) != 1 || spec.AccessRules[0].Sources[0] != "198.51.100.2/32" || !spec.Inbounds[0].Transport.AcceptProxyProtocol {
				t.Fatalf("landing %+v", spec)
			}
		}
	}
	if code, _ := c.do("PUT", "/inbounds/in/publication", map[string]any{"relayOnly": false}); code != 400 {
		t.Fatal("guard allowed direct publication", code)
	}
	// A missing boolean must not accidentally lift the access policy.
	if code, _ := c.do("PUT", "/inbounds/in/relay-access", map[string]any{}); code != 400 {
		t.Fatal("empty body lifts guard", code)
	}
	if code, out := c.do("POST", "/inbounds/in/relays/preview", map[string]any{"mode": "tunnel", "relays": []any{map[string]any{"nodeId": "relay"}}}); code != 400 {
		t.Fatal("unmatched proxy tunnel accepted", code, out)
	}
	if code, out := c.do("DELETE", "/inbounds/in/relays/"+id, nil); code != 204 {
		t.Fatal(code, out)
	}
	spec, e := s.BuildSpec("land")
	if e != nil || len(spec.AccessRules) != 1 || len(spec.AccessRules[0].Sources) != 0 {
		t.Fatalf("last line deletion lifted guard %+v %v", spec, e)
	}
	if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": false}); code != 200 {
		t.Fatal(code, out)
	}
	spec, e = s.BuildSpec("land")
	if e != nil || len(spec.AccessRules) != 0 || spec.Inbounds[0].Transport.AcceptProxyProtocol {
		t.Fatalf("explicit release %+v %v", spec, e)
	}
	var in store.Inbound
	s.st.DB.First(&in, "id = ?", "in")
	if !in.RelayOnly {
		t.Fatal("releasing firewall also published direct")
	}
}

func TestRelayAccessValidationAndChanges(t *testing.T) {
	s, c, _ := relayAccessFixture(t)
	s.st.DB.Model(&store.Node{}).Where("id = ?", "middle").Update("agent_features", store.NewJSON([]string{}))
	if code, _ := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true}); code != 400 {
		t.Fatal("old receiver accepted", code)
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "middle").Update("agent_features", store.NewJSON([]string{model.RelayAccessFeature}))
	if code, _ := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": false, "proxyProtocol": 2}); code != 400 {
		t.Fatal("proxy without guard", code)
	}
	if code, _ := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true, "sourceOverrides": map[string][]string{"relay": {"not-an-ip"}}}); code != 400 {
		t.Fatal("bad CIDR", code)
	}
	if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true, "sourceOverrides": map[string][]string{"relay": {"203.0.113.9/32", "2001:db8::9/128"}}}); code != 200 {
		t.Fatal(code, out)
	}
	spec, e := s.BuildSpec("middle")
	if e != nil || len(spec.AccessRules[0].Sources) != 2 {
		t.Fatal(spec, e)
	}
	var n store.Node
	s.st.DB.First(&n, "id = ?", "middle")
	s.observePublicIP(&n, "203.0.113.10", "")
	spec, e = s.BuildSpec("land")
	if e != nil || len(spec.AccessRules[0].Sources) != 1 || spec.AccessRules[0].Sources[0] != "203.0.113.10/32" {
		t.Fatalf("stale source %+v %v", spec, e)
	}
	s.dirtyMu.Lock()
	_, dirty := s.dirty["land"]
	s.dirtyMu.Unlock()
	if !dirty {
		t.Fatal("IP change did not republish landing")
	}
	// Older editors must preserve the independent access policy.
	code, exported := c.do("GET", "/inbounds/in/export", nil)
	if code != 200 {
		t.Fatal(code)
	}
	if code, out := c.do("PUT", "/inbounds/in", exported); code != 200 {
		t.Fatal(code, out)
	}
	var in store.Inbound
	s.st.DB.First(&in, "id = ?", "in")
	if !in.RelayAccess.V.Enabled {
		t.Fatal("editor lost access policy")
	}
}

func TestRelayAccessFetchNegotiation(t *testing.T) {
	s, _, _ := relayAccessFixture(t)
	s.st.DB.Model(&store.Node{}).Where("id = ?", "land").Updates(map[string]any{"cert_serial": "1", "desired_rev": 7})
	b, _ := json.Marshal(model.NodeSpec{NodeID: "land", Rev: 7, AccessRules: []model.AccessRule{{Protocol: "tcp", Port: 443, Sources: []string{"198.51.100.2/32"}}}})
	s.st.DB.Create(&store.ConfigRevision{NodeID: "land", Rev: 7, Spec: s.st.Box.Seal(string(b))})
	ctx := context.WithValue(context.Background(), peerCertKey, &x509.Certificate{Subject: pkix.Name{CommonName: "land"}, SerialNumber: big.NewInt(1)})
	service := &agentService{s: s}
	req := connect.NewRequest(&polarisv1.GetSpecRequest{Rev: 7})
	if _, e := service.GetSpec(ctx, req); connect.CodeOf(e) != connect.CodeFailedPrecondition {
		t.Fatalf("old agent fetched protected spec: %v", e)
	}
	req.Header().Set("X-Polaris-Agent-Features", model.RelayAccessFeature)
	if res, e := service.GetSpec(ctx, req); e != nil || !strings.Contains(string(res.Msg.SpecJson), "accessRules") {
		t.Fatal(res, e)
	}
}

func TestRelayAccessRejectsSharedFallbackAndRevokedRelease(t *testing.T) {
	s, c, _ := relayAccessFixture(t)
	var in store.Inbound
	s.st.DB.First(&in, "id = ?", "in")
	in.RelayAccess = store.NewJSON(store.RelayAccessPolicy{Enabled: true})
	in.Protocol.V.Fallbacks = []model.Fallback{{InboundID: "child"}}
	if e := validateRelayAccessInbound(s.st.DB, in); e == nil {
		t.Fatal("shared fallback master accepted")
	}
	in.Protocol.V.Fallbacks = nil
	parent := store.Inbound{ID: "parent", NodeID: in.NodeID, Tag: "parent", Port: 24444,
		Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS, Fallbacks: []model.Fallback{{InboundID: in.ID}}})}
	if e := s.st.DB.Create(&parent).Error; e != nil {
		t.Fatal(e)
	}
	if e := validateRelayAccessInbound(s.st.DB, in); e == nil {
		t.Fatal("shared fallback child accepted")
	}
	s.st.DB.Delete(&parent)
	if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true}); code != 200 {
		t.Fatal(code, out)
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "middle").Update("revoked", true)
	if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": false}); code != 200 {
		t.Fatalf("revoked relay prevented explicit release: %d %v", code, out)
	}
}

func TestRelayAccessCopyPreservesDenyAll(t *testing.T) {
	s, c, _ := relayAccessFixture(t)
	if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true, "proxyProtocol": 2}); code != 200 {
		t.Fatal(code, out)
	}
	if code, out := c.do("POST", "/inbounds/in/clone", map[string]any{"port": 24445}); code != 200 {
		t.Fatal(code, out)
	}
	code, exported := c.do("GET", "/inbounds/in/export", nil)
	if code != 200 {
		t.Fatal(code, exported)
	}
	exported["tag"], exported["port"] = "imported-guard", 24446
	if code, out := c.do("POST", "/inbounds", exported); code != 201 {
		t.Fatal(code, out)
	}
	var rows []store.Inbound
	if e := s.st.DB.Where("node_id = ? AND id <> ?", "land", "in").Find(&rows).Error; e != nil || len(rows) != 2 {
		t.Fatalf("copy failed: %+v %v", rows, e)
	}
	for _, in := range rows {
		if !in.RelayOnly || !in.RelayAccess.V.Enabled || in.RelayAccess.V.ProxyProtocol != 2 {
			t.Fatalf("copy lost policy: %+v", in)
		}
	}
	spec, e := s.BuildSpec("land")
	if e != nil {
		t.Fatal(e)
	}
	denied := 0
	for _, r := range spec.AccessRules {
		if r.Port == 24445 || r.Port == 24446 {
			if len(r.Sources) != 0 {
				t.Fatal("copy inherited source trust", r)
			}
			denied++
		}
	}
	if denied != 2 {
		t.Fatal("copies not guarded", spec.AccessRules)
	}
}

func TestRelayAccessTunnelReceiverPorts(t *testing.T) {
	s, c, old := relayAccessFixture(t)
	if code, out := c.do("DELETE", "/inbounds/in/relays/"+old, nil); code != 204 {
		t.Fatal(code, out)
	}
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"mode": "tunnel", "transport": "ws", "relays": []any{map[string]any{"nodeId": "relay"}, map[string]any{"nodeId": "middle"}}}))
	if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true}); code != 200 {
		t.Fatal(code, out)
	}
	var ch store.Chain
	s.st.DB.First(&ch, "id = ?", id)
	for i, nodeID := range []string{"middle", "land"} {
		spec, e := s.BuildSpec(nodeID)
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, r := range spec.AccessRules {
			if r.Port == ch.Hops.V[i+1].TunnelPort && r.Protocol == "tcp" && len(r.Sources) == 1 {
				found = true
			}
			if nodeID == "land" && r.Port == 443 && len(r.Sources) != 0 {
				t.Fatal("tunnel exposed direct landing", r)
			}
		}
		if !found {
			t.Fatal("tunnel receiver not guarded", nodeID, spec.AccessRules)
		}
	}
}
