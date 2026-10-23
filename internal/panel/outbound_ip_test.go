package panel

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"testing"

	"connectrpc.com/connect"
	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func outboundIPFixture(t *testing.T) (*Server, *apiClient) {
	s, c := relayFixture(t)
	s.st.DB.Model(&store.Core{}).Where("node_id = ?", "land").Update("version", "26.5.9")
	s.st.DB.Model(&store.Node{}).Where("id = ?", "land").Update("agent_features", store.NewJSON([]string{model.OutboundIPFeature}))
	return s, c
}
func TestOutboundIPLifecycle(t *testing.T) {
	s, c := outboundIPFixture(t)
	for _, req := range []any{map[string]any{}, map[string]any{"strategy": "bogus"}} {
		if code, _ := c.do("PUT", "/inbounds/in/outbound-ip", req); code != 400 {
			t.Fatal(code)
		}
	}
	if code, out := c.do("PUT", "/inbounds/in/outbound-ip", map[string]any{"strategy": "ipv6_only"}); code != 200 {
		t.Fatal(code, out)
	}
	spec, e := s.BuildSpec("land")
	if e != nil || spec.Inbounds[0].OutboundIPStrategy != model.OutboundIPv6Only {
		t.Fatal(e, spec)
	}
	code, exp := c.do("GET", "/inbounds/in/export", nil)
	if code != 200 || exp["outboundIPStrategy"] != "ipv6_only" {
		t.Fatal(code, exp)
	}
	// Older editors omit this independent option.
	delete(exp, "outboundIPStrategy")
	exp["tag"] = "renamed"
	if code, out := c.do("PUT", "/inbounds/in", exp); code != 200 {
		t.Fatal(code, out)
	}
	var row store.Inbound
	s.st.DB.First(&row, "id = ?", "in")
	if row.OutboundIPStrategy != model.OutboundIPv6Only {
		t.Fatal("old editor erased policy")
	}
	if code, out := c.do("POST", "/inbounds/in/clone", map[string]any{"port": 24445}); code != 200 {
		t.Fatal(code, out)
	}
	exp["tag"], exp["port"], exp["outboundIPStrategy"] = "imported", 24446, "ipv6_only"
	if code, out := c.do("POST", "/inbounds", exp); code != 201 {
		t.Fatal(code, out)
	}
	var rows []store.Inbound
	s.st.DB.Where("node_id = ?", "land").Find(&rows)
	if len(rows) != 3 {
		t.Fatal(len(rows))
	}
	for _, r := range rows {
		if r.OutboundIPStrategy != model.OutboundIPv6Only {
			t.Fatal("copy lost policy", r.ID)
		}
	}
	if code, out := c.do("PUT", "/inbounds/in/outbound-ip", map[string]any{"strategy": "auto"}); code != 200 {
		t.Fatal(code, out)
	}
}
func TestOutboundIPCompatibility(t *testing.T) {
	s, c := outboundIPFixture(t)
	s.st.DB.Model(&store.Core{}).Where("node_id = ?", "land").Update("version", "26.3.27")
	if code, _ := c.do("PUT", "/inbounds/in/outbound-ip", map[string]any{"strategy": "ipv4_only"}); code != 400 {
		t.Fatal("old core accepted", code)
	}
	s.st.DB.Model(&store.Core{}).Where("node_id = ?", "land").Update("version", "26.5.9")
	s.st.DB.Model(&store.Node{}).Where("id = ?", "land").Update("agent_features", store.NewJSON([]string{model.RelayAccessFeature}))
	if code, _ := c.do("PUT", "/inbounds/in/outbound-ip", map[string]any{"strategy": "prefer_ipv4"}); code != 400 {
		t.Fatal("old agent accepted", code)
	}
}
func TestOutboundIPFetchNegotiation(t *testing.T) {
	s, _ := outboundIPFixture(t)
	s.st.DB.Model(&store.Node{}).Where("id = ?", "land").Updates(map[string]any{"cert_serial": "1", "desired_rev": 7})
	raw, _ := json.Marshal(model.NodeSpec{NodeID: "land", Rev: 7, Inbounds: []model.Inbound{{Enabled: true, OutboundIPStrategy: model.OutboundIPv4Only}}})
	s.st.DB.Create(&store.ConfigRevision{NodeID: "land", Rev: 7, Spec: s.st.Box.Seal(string(raw))})
	ctx := context.WithValue(context.Background(), peerCertKey, &x509.Certificate{Subject: pkix.Name{CommonName: "land"}, SerialNumber: big.NewInt(1)})
	service := &agentService{s: s}
	req := connect.NewRequest(&polarisv1.GetSpecRequest{Rev: 7})
	req.Header().Set("X-Polaris-Agent-Features", model.RelayAccessFeature)
	if _, e := service.GetSpec(ctx, req); connect.CodeOf(e) != connect.CodeFailedPrecondition {
		t.Fatal("old agent fetched IP policy", e)
	}
	req.Header().Set("X-Polaris-Agent-Features", model.OutboundIPFeature)
	if _, e := service.GetSpec(ctx, req); e != nil {
		t.Fatal(e)
	}
	// Outbound IP support is independent of the source ACL feature.
	raw, _ = json.Marshal(model.NodeSpec{NodeID: "land", Rev: 8, AccessRules: []model.AccessRule{{Port: 443}}})
	s.st.DB.Create(&store.ConfigRevision{NodeID: "land", Rev: 8, Spec: s.st.Box.Seal(string(raw))})
	req.Msg.Rev = 8
	if _, e := service.GetSpec(ctx, req); connect.CodeOf(e) != connect.CodeFailedPrecondition {
		t.Fatal("IP-only agent fetched firewall policy", e)
	}
}
func TestOutboundIPDNSChanges(t *testing.T) {
	s, c := outboundIPFixture(t)
	if code, out := c.do("PUT", "/inbounds/in/outbound-ip", map[string]any{"strategy": "ipv4_only"}); code != 200 {
		t.Fatal(code, out)
	}
	if e := s.checkOutboundIPDNSChange(model.GlobalNodeID, "on", &model.DNSSpec{Strategy: "ipv4_only"}); e == nil {
		t.Fatal("incompatible inherited DNS allowed")
	}
	s.st.DB.Create(&store.DNSConfig{Scope: "land", Mode: "off"})
	if e := s.checkOutboundIPDNSChange(model.GlobalNodeID, "on", &model.DNSSpec{Strategy: "ipv4_only"}); e != nil {
		t.Fatal("node override ignored", e)
	}
	s.st.DB.Create(&store.DNSConfig{Scope: model.GlobalNodeID, Mode: "on", Spec: store.NewJSON(model.DNSSpec{Strategy: "ipv6_only"})})
	if e := s.checkOutboundIPDNSChange("land", "inherit", nil); e == nil {
		t.Fatal("incompatible inherit allowed")
	}
}

func TestOutboundIPTemplateCompatibilityAndSync(t *testing.T) {
	s, c := outboundIPFixture(t)
	item := store.TemplateItem{Key: "entry", Core: model.CoreXray, Protocol: model.ProtocolSpec{Type: model.ProtoVLESS}, Transport: model.TransportSpec{Type: model.TransTCP}, Security: model.SecuritySpec{Type: model.SecNone}, Port: store.PortPolicy{Mode: "fixed", Fixed: 24448}, Cert: "none", TagPattern: "generated-{node.name}", OutboundIPStrategy: model.OutboundIPv6Only}
	tpl := store.InboundTemplate{ID: "tpl-ip", Name: "IP policy", Items: store.NewJSON([]store.TemplateItem{item})}
	if e := s.st.DB.Create(&tpl).Error; e != nil {
		t.Fatal(e)
	}
	var n store.Node
	s.st.DB.First(&n, "id = ?", "land")
	plan := s.planNode(&tpl, &n, &planOptions{}, false)
	if len(plan.Items) != 1 || plan.Items[0].Result == "skip" || plan.Items[0].inbound.OutboundIPStrategy != model.OutboundIPv6Only {
		t.Fatalf("template did not generate policy: %+v", plan)
	}
	// An older template editor must preserve policy when omitting its field.
	raw, _ := json.Marshal(item)
	var old map[string]any
	json.Unmarshal(raw, &old)
	delete(old, "outboundIPStrategy")
	if code, out := c.do("PUT", "/templates/"+tpl.ID, map[string]any{"name": tpl.Name, "items": []any{old}}); code != 200 {
		t.Fatal(code, out)
	}
	s.st.DB.First(&tpl, "id = ?", tpl.ID)
	if tpl.Items.V[0].OutboundIPStrategy != model.OutboundIPv6Only {
		t.Fatal("old editor erased template policy")
	}
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Updates(map[string]any{"template_id": tpl.ID, "template_item_key": "entry", "port": 24448})
	url := "/templates/" + tpl.ID + "/sync"
	if code, out := c.do("POST", url, map[string]any{"confirm": true, "selections": map[string][]string{"in": {"outboundIPStrategy"}}}); code != 200 {
		t.Fatal(code, out)
	}
	var in store.Inbound
	s.st.DB.First(&in, "id = ?", "in")
	if in.OutboundIPStrategy != model.OutboundIPv6Only {
		t.Fatal("sync did not apply policy")
	}
	if code, out := c.do("PUT", "/inbounds/in/outbound-ip", map[string]any{"strategy": "prefer_ipv4"}); code != 200 {
		t.Fatal(code, out)
	}
	if code, out := c.do("POST", url, map[string]any{"confirm": true, "selections": map[string][]string{"in": {"outboundIPStrategy"}}}); code != 200 {
		t.Fatal(code, out)
	}
	s.st.DB.First(&in, "id = ?", "in")
	if in.OutboundIPStrategy != model.OutboundPreferIPv4 || !contains(in.OverriddenFields.V, "outboundIPStrategy") {
		t.Fatal("template erased manual override")
	}
}
