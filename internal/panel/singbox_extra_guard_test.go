package panel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestSingboxProxyProtocolExtraRejectedAtSave guards the rev-23 startup crash
// at the source: a raw Extra with a removed PROXY-protocol key must be rejected
// when a sing-box landing inbound is configured, rather than being persisted
// and later crashing the core with
// "Proxy Protocol is deprecated and removed in sing-box 1.6.0".
func TestSingboxProxyProtocolExtraRejectedAtSave(t *testing.T) {
	s, _, _ := newAdminClient(t)
	if e := s.st.DB.Create(&store.Node{ID: "sbnode", Name: "sbnode", Address: "sb.example.com", Status: "online"}).Error; e != nil {
		t.Fatal(e)
	}
	if e := s.st.DB.Create(&store.Core{ID: "sb", NodeID: "sbnode", Type: model.CoreSingbox, Version: "1.13.5"}).Error; e != nil {
		t.Fatal(e)
	}
	node := store.Node{ID: "sbnode", DefaultCore: model.CoreSingbox}

	for _, key := range []string{"proxy_protocol", "proxy_protocol_accept_no_header", "acceptProxyProtocol"} {
		extra, _ := json.Marshal(map[string]any{key: true})
		m := model.Inbound{
			NodeID:    "sbnode",
			Core:      model.CoreSingbox,
			Tag:       "anytls-" + key,
			Port:      22000,
			Protocol:  model.ProtocolSpec{Type: model.ProtoAnyTLS},
			Transport: model.TransportSpec{Type: model.TransTCP},
			Security:  model.SecuritySpec{Type: model.SecTLS},
			Extra:     extra,
			Enabled:   true,
		}
		err := s.validateInbound(&node, &m, "")
		if err == nil || !strings.Contains(err.Error(), "PROXY protocol") {
			t.Fatalf("sing-box Extra with %s was not rejected: %v", key, err)
		}
	}

	// A clean Extra on the same sing-box inbound is accepted.
	m := model.Inbound{
		NodeID:    "sbnode",
		Core:      model.CoreSingbox,
		Tag:       "anytls-clean",
		Port:      22100,
		Protocol:  model.ProtocolSpec{Type: model.ProtoAnyTLS},
		Transport: model.TransportSpec{Type: model.TransTCP},
		Security:  model.SecuritySpec{Type: model.SecTLS},
		Extra:     json.RawMessage(`{"sniffing":{"routeOnly":true}}`),
		Enabled:   true,
	}
	if err := s.validateInbound(&node, &m, ""); err != nil {
		t.Fatalf("clean sing-box Extra rejected: %v", err)
	}
}
