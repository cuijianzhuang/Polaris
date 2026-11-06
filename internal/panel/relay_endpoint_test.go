package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestRelayEndpointPreservesLandingTLSName(t *testing.T) {
	for _, explicit := range []string{"", "certificate.example.com"} {
		t.Run(explicit, func(t *testing.T) {
			s, c := relayFixture(t)
			u := relayTestUser(t, s)
			// An address can be a hostname without a separate node domain.
			s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Updates(map[string]any{
				"core": model.CoreSingbox, "protocol": store.NewJSON(model.ProtocolSpec{Type: model.ProtoAnyTLS}),
				"security":   store.NewJSON(model.SecuritySpec{Type: model.SecTLS, ServerName: explicit}),
				"relay_only": true,
			})
			createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
			nodes, err := s.subNodes(&u)
			if err != nil || len(nodes) != 1 {
				t.Fatal(nodes, err)
			}
			n := nodes[0]
			wantSNI := explicit
			if wantSNI == "" {
				wantSNI = "land.example.com"
			}
			if n.Server != "relay.example.com" || n.TLS.ServerName != wantSNI || n.Password != u.Password || n.Protocol != model.ProtoAnyTLS || n.TLS.Insecure {
				t.Fatalf("relay must preserve the landing's TLS identity and authentication: %+v", n)
			}
			if err := s.st.DB.Model(&store.Endpoint{}).Where("inbound_id = ? AND kind = ?", "in", "relay").Update("sni", "override.example.com").Error; err != nil {
				t.Fatal(err)
			}
			nodes, err = s.subNodes(&u)
			if err != nil || len(nodes) != 1 || nodes[0].TLS.ServerName != "override.example.com" {
				t.Fatal("explicit endpoint SNI must take precedence", nodes, err)
			}
		})
	}
}

func TestRelayRealityKeyConsistency(t *testing.T) {
	s, c := relayFixture(t)
	u := relayTestUser(t, s)
	private, public, err := pki.RealityKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	_, wrong, err := pki.RealityKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	sec := model.SecuritySpec{Type: model.SecReality, RealityDest: "www.example.com:443", RealityServerNames: []string{"www.example.com"}, RealityShortIDs: []string{"0123456789abcdef"}, RealityPrivateKey: s.st.Box.Seal(private), RealityPublicKey: wrong}
	if err := s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Updates(map[string]any{"security": store.NewJSON(sec), "relay_only": true}).Error; err != nil {
		t.Fatal(err)
	}
	createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	nodes, err := s.subNodes(&u)
	if err != nil || len(nodes) != 1 {
		t.Fatal(nodes, err)
	}
	n := nodes[0]
	if n.TLS.PublicKey != public || n.TLS.ServerName != "www.example.com" || n.TLS.ShortID != "0123456789abcdef" || n.Server != "relay.example.com" {
		t.Fatal("relay subscription must match the landing's private key, SNI and short ID")
	}
	if code, out := c.do("GET", "/inbounds/in", nil); code != 200 || out["security"].(map[string]any)["realityPublicKey"] != public || out["security"].(map[string]any)["realityPrivateKey"] != nil {
		t.Fatal("inbound view must expose the matching public key without its private half", code)
	}
	// Replacing a private key must not retain the former public half, and an
	// invalid private key must be rejected even when a public key was supplied.
	m := model.Inbound{Tag: "key-test", Port: 443, Protocol: model.ProtocolSpec{Type: model.ProtoVLESS}, Security: model.SecuritySpec{Type: model.SecReality, RealityDest: "www.example.com:443", RealityPrivateKey: private, RealityPublicKey: wrong}}
	if err := s.prepareInbound(&m); err != nil || m.Security.RealityPublicKey != public {
		t.Fatal("public half was not derived from replacement private key", err)
	}
	m.Security.RealityPrivateKey = "invalid-private-key"
	if err := s.prepareInbound(&m); err == nil || !strings.Contains(err.Error(), "reality private key") {
		t.Fatal("invalid REALITY private key accepted with a public key")
	}
}

func TestRelayEndpointRejectsUnusableBinding(t *testing.T) {
	s, c := relayFixture(t)
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	path := "/inbounds/in/endpoints"
	for _, port := range []int{-1, 65536} {
		if code, out := c.do("POST", path, map[string]any{"kind": "relay", "relayChainId": id, "port": port}); code != 400 {
			t.Fatalf("invalid endpoint port accepted: %d %v", code, out)
		}
	}
	s.st.DB.Model(&store.Chain{}).Where("id = ?", id).Update("disabled", true)
	if code, out := c.do("POST", path, map[string]any{"kind": "relay", "relayChainId": id}); code != 400 {
		t.Fatalf("disabled chain accepted: %d %v", code, out)
	}
	var ch store.Chain
	s.st.DB.First(&ch, "id = ?", id)
	ch.Disabled = false
	ch.Hops.V[len(ch.Hops.V)-1].NodeID = "middle"
	s.st.DB.Save(&ch)
	if code, out := c.do("POST", path, map[string]any{"kind": "relay", "relayChainId": id}); code != 400 {
		t.Fatalf("chain pointing at another node accepted: %d %v", code, out)
	}
	u := relayTestUser(t, s)
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("relay_only", true)
	if nodes, err := s.subNodes(&u); err != nil || len(nodes) != 0 {
		t.Fatal("invalid landing published from a stored endpoint", nodes, err)
	}
}
