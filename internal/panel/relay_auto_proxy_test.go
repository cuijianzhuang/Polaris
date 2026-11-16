package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// autoProxyServer builds the relay/middle/land nodes of relayFixture with
// agents that report relay access support and public IPs, as a current
// deployment does. No relay access policy is configured: the whole point is
// that the L4 chain must carry the client IP without manual setup.
func autoProxyServer(t *testing.T) (*Server, *apiClient) {
	t.Helper()
	s, c := relayFixture(t)
	for id, ip := range map[string]string{"relay": "198.51.100.1", "middle": "198.51.100.2", "land": "198.51.100.3"} {
		if e := s.st.DB.Model(&store.Node{}).Where("id = ?", id).Updates(map[string]any{"public_ipv4": ip, "agent_features": store.NewJSON([]string{model.RelayAccessFeature})}).Error; e != nil {
			t.Fatal(e)
		}
	}
	return s, c
}

func createAutoProxyChain(t *testing.T, c *apiClient, mode string) string {
	t.Helper()
	body := map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}, map[string]any{"nodeId": "middle"}}}
	if mode != "" {
		body["mode"] = mode
		if mode == "tunnel" {
			body["transport"] = "ws"
		}
	}
	return createRelayTest(t, c, previewRelayTest(t, c, body))
}

func landingAcceptsProxy(t *testing.T, s *Server) bool {
	t.Helper()
	spec, e := s.BuildSpec("land")
	if e != nil {
		t.Fatal(e)
	}
	if len(spec.Inbounds) != 1 {
		t.Fatalf("landing inbounds %+v", spec.Inbounds)
	}
	return spec.Inbounds[0].Transport.AcceptProxyProtocol
}

func forwardSendProxy(t *testing.T, s *Server, node string) (send int, accept bool) {
	t.Helper()
	spec, e := s.BuildSpec(node)
	if e != nil {
		t.Fatal(e)
	}
	if len(spec.Forwards) != 1 {
		t.Fatalf("%s forwards %+v", node, spec.Forwards)
	}
	return spec.Forwards[0].SendProxy, spec.Forwards[0].AcceptProxy
}

// TestL4AutoProxyProtocol: an L4 passthrough chain landing on a compatible
// Xray TCP inbound carries the client IP with PROXY protocol even when the
// operator never opened relay access.
func TestL4AutoProxyProtocol(t *testing.T) {
	s, c := autoProxyServer(t)
	id := createAutoProxyChain(t, c, "")
	u := relayTestUser(t, s)

	if !landingAcceptsProxy(t, s) {
		t.Fatal("landing did not auto-enable AcceptProxyProtocol")
	}
	if send, accept := forwardSendProxy(t, s, "relay"); send != 1 || accept {
		t.Fatalf("entry forward send=%d accept=%v, want 1/false", send, accept)
	}
	if send, accept := forwardSendProxy(t, s, "middle"); send != 1 || !accept {
		t.Fatalf("middle forward send=%d accept=%v, want 1/true", send, accept)
	}
	// The direct entry must not be published anymore: a direct handshake
	// would be parsed as a PROXY header and rejected.
	subs, e := s.subNodes(&u)
	if e != nil || len(subs) != 1 || subs[0].Server != "relay.example.com" {
		t.Fatalf("direct entry still published: %+v %v", subs, e)
	}
	if targets := s.probeTargets(""); len(targets) != 0 {
		t.Fatalf("auto-PROXY landing still directly probed: %+v", targets)
	}

	// Disabling the chain restores the direct path.
	if code, _ := c.do("PUT", "/inbounds/in/relays/"+id+"/state", map[string]any{"enabled": false}); code != 200 {
		t.Fatal(code)
	}
	if landingAcceptsProxy(t, s) {
		t.Fatal("disabled chain still enables AcceptProxyProtocol")
	}
	subs, e = s.subNodes(&u)
	if e != nil || len(subs) != 1 || subs[0].Server != "land.example.com" {
		t.Fatalf("direct entry not restored after disable: %+v %v", subs, e)
	}
	if targets := s.probeTargets(""); len(targets) != 1 {
		t.Fatalf("landing not probed after disable: %+v", targets)
	}

	// Re-enabling restores the automatic PROXY pairing.
	if code, _ := c.do("PUT", "/inbounds/in/relays/"+id+"/state", map[string]any{"enabled": true}); code != 200 {
		t.Fatal(code)
	}
	if !landingAcceptsProxy(t, s) {
		t.Fatal("re-enabled chain did not restore AcceptProxyProtocol")
	}
	// Deleting the chain restores the direct path as well.
	if code, _ := c.do("DELETE", "/inbounds/in/relays/"+id, nil); code != 204 {
		t.Fatal(code)
	}
	if landingAcceptsProxy(t, s) {
		t.Fatal("deleted chain still enables AcceptProxyProtocol")
	}
	subs, e = s.subNodes(&u)
	if e != nil || len(subs) != 1 || subs[0].Server != "land.example.com" {
		t.Fatalf("direct entry not restored after delete: %+v %v", subs, e)
	}
}

// TestL4AutoProxyRespectsExplicitVersion: an explicitly configured PROXY
// protocol version is never downgraded to the automatic 1, and a source
// guard without an explicit version still gets the automatic pairing.
func TestL4AutoProxyRespectsExplicitVersion(t *testing.T) {
	s, c := autoProxyServer(t)
	createAutoProxyChain(t, c, "")
	if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true, "proxyProtocol": 2}); code != 200 {
		t.Fatal(code, out)
	}
	if !landingAcceptsProxy(t, s) {
		t.Fatal("explicit proxy 2 lost AcceptProxyProtocol")
	}
	if send, accept := forwardSendProxy(t, s, "relay"); send != 2 || accept {
		t.Fatalf("entry forward send=%d accept=%v, want 2/false", send, accept)
	}
	if send, accept := forwardSendProxy(t, s, "middle"); send != 2 || !accept {
		t.Fatalf("middle forward send=%d accept=%v, want 2/true", send, accept)
	}
	// Guard only, no explicit version: the automatic version fills the gap.
	if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true}); code != 200 {
		t.Fatal(code, out)
	}
	if !landingAcceptsProxy(t, s) {
		t.Fatal("guard-only landing lost AcceptProxyProtocol")
	}
	if send, _ := forwardSendProxy(t, s, "relay"); send != 1 {
		t.Fatalf("guard-only entry forward send=%d, want 1", send)
	}
}

// TestL4AutoProxySkipsProxyMode: a proxy-mode chain landing on the same
// inbound must not enable PROXY protocol — a protocol outbound cannot emit
// a PROXY header, so the landing would parse the VLESS handshake as one
// and drop the connection.
func TestL4AutoProxySkipsProxyMode(t *testing.T) {
	s, c := autoProxyServer(t)
	id := createAutoProxyChain(t, c, "")
	// Flip the stored mode: the chain still lands on the same inbound, but
	// it is now a proxy chain rather than an L4 passthrough chain.
	if e := s.st.DB.Model(&store.Chain{}).Where("id = ?", id).Update("mode", "proxy").Error; e != nil {
		t.Fatal(e)
	}
	if landingAcceptsProxy(t, s) {
		t.Fatal("proxy chain landing auto-enabled AcceptProxyProtocol")
	}
	spec, e := s.BuildSpec("relay")
	if e != nil || len(spec.Forwards) != 0 {
		t.Fatalf("proxy chain produced L4 forwards: %+v %v", spec.Forwards, e)
	}
	u := relayTestUser(t, s)
	subs, e := s.subNodes(&u)
	if e != nil || len(subs) != 1 || subs[0].Server != "land.example.com" {
		t.Fatalf("proxy landing lost its direct entry: %+v %v", subs, e)
	}
}

func TestL4AutoProxySkipsIncompatibleLanding(t *testing.T) {
	t.Run("tunnel", func(t *testing.T) {
		s, c := autoProxyServer(t)
		createAutoProxyChain(t, c, "tunnel")
		if landingAcceptsProxy(t, s) {
			t.Fatal("tunnel chain landing auto-enabled AcceptProxyProtocol")
		}
		for _, node := range []string{"relay", "middle", "land"} {
			spec, e := s.BuildSpec(node)
			if e != nil {
				t.Fatal(e)
			}
			for _, f := range spec.Forwards {
				if f.SendProxy != 0 {
					t.Fatalf("%s tunnel forward sends PROXY: %+v", node, f)
				}
			}
		}
		// The direct entry keeps working: a tunnel does not put a PROXY
		// header on the landing stream.
		u := relayTestUser(t, s)
		subs, e := s.subNodes(&u)
		if e != nil || len(subs) != 2 {
			t.Fatalf("tunnel landing lost its direct entry: %+v %v", subs, e)
		}
	})
	t.Run("singbox core", func(t *testing.T) {
		s, c := autoProxyServer(t)
		createAutoProxyChain(t, c, "")
		if e := s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("core", model.CoreSingbox).Error; e != nil {
			t.Fatal(e)
		}
		if landingAcceptsProxy(t, s) {
			t.Fatal("sing-box landing auto-enabled AcceptProxyProtocol")
		}
		if send, _ := forwardSendProxy(t, s, "relay"); send != 0 {
			t.Fatalf("entry forward send=%d, want 0", send)
		}
	})
	t.Run("ws transport", func(t *testing.T) {
		s, c := autoProxyServer(t)
		createAutoProxyChain(t, c, "")
		if e := s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("transport", store.NewJSON(model.TransportSpec{Type: model.TransWS})).Error; e != nil {
			t.Fatal(e)
		}
		if landingAcceptsProxy(t, s) {
			t.Fatal("WS landing auto-enabled AcceptProxyProtocol")
		}
		if send, _ := forwardSendProxy(t, s, "relay"); send != 0 {
			t.Fatalf("entry forward send=%d, want 0", send)
		}
	})
	t.Run("old agent", func(t *testing.T) {
		s, c := autoProxyServer(t)
		createAutoProxyChain(t, c, "")
		if e := s.st.DB.Model(&store.Node{}).Where("id = ?", "land").Update("agent_features", store.NewJSON([]string{})).Error; e != nil {
			t.Fatal(e)
		}
		if landingAcceptsProxy(t, s) {
			t.Fatal("landing without relay-access agent auto-enabled AcceptProxyProtocol")
		}
		if send, _ := forwardSendProxy(t, s, "relay"); send != 0 {
			t.Fatalf("entry forward send=%d, want 0", send)
		}
	})
	t.Run("unknown core", func(t *testing.T) {
		s, c := autoProxyServer(t)
		createAutoProxyChain(t, c, "")
		if e := s.st.DB.Where("node_id = ?", "land").Delete(&store.Core{}).Error; e != nil {
			t.Fatal(e)
		}
		if landingAcceptsProxy(t, s) {
			t.Fatal("landing without a known core auto-enabled AcceptProxyProtocol")
		}
		if send, _ := forwardSendProxy(t, s, "relay"); send != 0 {
			t.Fatalf("entry forward send=%d, want 0", send)
		}
	})
}
