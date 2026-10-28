package panel

import (
	"slices"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// An inbound that cannot go through a CDN gets a VLESS + WS + TLS sibling on
// a free CDN port, for the same groups, published only through the profile.
func TestCDNSiblingInbound(t *testing.T) {
	s, _, admin := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "jp", Address: "203.0.113.5"})
	db.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"})
	db.Create(&store.Inbound{ID: "re", NodeID: "n1", Tag: "reality", Port: 443, Core: model.CoreXray, Enabled: true, Remark: "主线路",
		Protocol:  store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"}),
		Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}),
		Security:  store.NewJSON(model.SecuritySpec{Type: model.SecReality, RealityDest: "www.microsoft.com:443"})})
	db.Create(&store.Group{ID: "vip", Name: "vip"})
	db.Create(&store.GroupInbound{GroupID: "vip", InboundID: "re"})
	db.Create(&store.CDNProfile{ID: "cf", Name: "cf-main", Provider: "cloudflare", OriginDomain: "cdn.example.com",
		Ports: store.NewJSON([]int{443}), PreferredIPs: store.NewJSON([]string{})})

	if code, _ := admin.do("POST", "/inbounds/re/cdn-sibling", map[string]any{"cdnProfileId": "nope"}); code != 400 {
		t.Fatalf("unknown profile: %d", code)
	}
	code, out := admin.do("POST", "/inbounds/re/cdn-sibling", map[string]any{"cdnProfileId": "cf"})
	if code != 201 || out["port"] != float64(2053) || out["addedPort"] != true {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["inbound"].(map[string]any)["id"].(string)
	var row store.Inbound
	db.First(&row, "id = ?", id)
	m := s.toModelInbound(&row)
	if row.Tag != "reality-cdn" || row.Remark != "CDN · 主线路" || m.Protocol.Type != model.ProtoVLESS || m.Transport.Type != model.TransWS ||
		m.Security.Type != model.SecTLS || m.Security.ServerName != "cdn.example.com" || m.Transport.Host != "cdn.example.com" ||
		len(m.Transport.Path) < 4 || m.Security.CertificateID == "" || !m.CDNCapable() {
		t.Fatalf("sibling %+v / %+v", row, m)
	}
	var eps []store.Endpoint
	db.Where("inbound_id = ?", id).Find(&eps)
	if len(eps) != 1 || eps[0].Kind != "cdn" || eps[0].CDNProfileID != "cf" {
		t.Fatalf("endpoints %+v", eps)
	}
	var groups []string
	db.Model(&store.GroupInbound{}).Where("inbound_id = ?", id).Pluck("group_id", &groups)
	if !slices.Equal(groups, []string{"vip"}) {
		t.Fatalf("groups %v", groups)
	}
	var p store.CDNProfile
	db.First(&p, "id = ?", "cf")
	if !slices.Equal(p.Ports.V, []int{443, 2053}) {
		t.Fatalf("profile ports %v", p.Ports.V)
	}

	// A second one takes the next free port and tag.
	_, out = admin.do("POST", "/inbounds/re/cdn-sibling", map[string]any{"cdnProfileId": "cf"})
	if out["port"] != float64(2083) || out["inbound"].(map[string]any)["tag"] != "reality-cdn-2" {
		t.Fatalf("second: %v", out)
	}
	// Every candidate taken: a clear error, nothing created.
	for i, port := range []int{2087, 2096, 8443} {
		db.Create(&store.Inbound{ID: "x" + string(rune('a'+i)), NodeID: "n1", Tag: "x" + string(rune('a'+i)), Port: port, Core: model.CoreXray, Enabled: true,
			Protocol:  store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}),
			Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}),
			Security:  store.NewJSON(model.SecuritySpec{Type: model.SecNone})})
	}
	var before int64
	db.Model(&store.Inbound{}).Count(&before)
	if code, out := admin.do("POST", "/inbounds/re/cdn-sibling", map[string]any{"cdnProfileId": "cf"}); code != 400 {
		t.Fatalf("no free port: %d %v", code, out)
	}
	var after int64
	db.Model(&store.Inbound{}).Count(&after)
	if after != before {
		t.Fatal("an inbound was created without a free port")
	}
}
