package panel

import (
	"bytes"
	"encoding/json"
	"github.com/cuijianzhuang/polaris/internal/secret"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func resourceFixture(t *testing.T) (*Server, *apiClient, *store.SubSource, []sub.Node) {
	t.Helper()
	s, _, c := newAdminClient(t)
	policyLicense(t, s, true)
	src := &store.SubSource{ID: "source", Name: "Provider", Kind: "manual", Enabled: true}
	if e := s.st.DB.Create(src).Error; e != nil {
		t.Fatal(e)
	}
	ns := []sub.Node{{Name: "First", Server: "one.example.com", Port: 443, Protocol: model.ProtoTrojan, Password: "upstream-password-one", Transport: model.TransportSpec{Type: model.TransTCP}, TLS: model.ClientTLS{Type: model.SecTLS, ServerName: "one.example.com"}}, {Name: "Second", Server: "two.example.com", Port: 443, Protocol: model.ProtoVLESS, UUID: "c3788c35-61f4-42c0-b980-e90a50e96a3b", Transport: model.TransportSpec{Type: model.TransTCP}, TLS: model.ClientTLS{Type: model.SecNone}}}
	out, e := s.commitSourceRefresh(src, ns, store.SubTraffic{})
	if e != nil {
		t.Fatal(e)
	}
	return s, c, src, out
}
func TestSourceResourceIdentityRotationAndDiff(t *testing.T) {
	s, _, src, ns := resourceFixture(t)
	ids := []string{ns[0].ResourceID, ns[1].ResourceID}
	ns[0].Name = "Renamed"
	ns[0].Password = "rotated-password"
	ns = []sub.Node{ns[1], ns[0]}
	out, e := s.commitSourceRefresh(src, ns, store.SubTraffic{})
	if e != nil {
		t.Fatal(e)
	}
	if out[0].ResourceID != ids[1] || out[1].ResourceID != ids[0] {
		t.Fatal("identity changed on reorder/rename/rotation", out)
	}
	var h store.SubSourceRefresh
	if e = s.st.DB.Order("revision DESC").First(&h).Error; e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(h)
	if len(h.Changes.V) != 1 || h.Changes.V[0].Kind != "updated" || !strings.Contains(string(b), "credentials") || strings.Contains(string(b), "password") {
		t.Fatal("invalid diff", string(b))
	}
	var row store.SubSourceNode
	s.st.DB.First(&row, "id = ?", ids[0])
	if !strings.HasPrefix(row.Payload, "enc:v1:") || strings.Contains(row.Payload, "rotated-password") || !strings.HasPrefix(src.Cache, "enc:v1:") {
		t.Fatal("plaintext at rest")
	}
	out, e = s.commitSourceRefresh(src, out[:1], store.SubTraffic{})
	if e != nil {
		t.Fatal(e)
	}
	s.st.DB.First(&row, "id = ?", ids[0])
	if !row.Missing {
		t.Fatal("removed node not retained")
	}
	n := ns[1]
	out, e = s.commitSourceRefresh(src, append(out, n), store.SubTraffic{})
	if e != nil {
		t.Fatal(e)
	}
	if out[1].ResourceID != ids[0] {
		t.Fatal("restoration lost identity")
	}
	old := *src
	if _, e = s.commitSourceRefresh(src, out, store.SubTraffic{}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.commitSourceRefresh(&old, ns[:1], store.SubTraffic{}); e == nil {
		t.Fatal("stale refresh overwrote current data")
	}
}
func TestSourceResourceAmbiguousRotationDoesNotRebind(t *testing.T) {
	s, _, src, ns := resourceFixture(t)
	ns[1] = ns[0]
	ns[1].Name = "Another account"
	ns[1].Password = "second-account"
	out, e := s.commitSourceRefresh(src, ns, store.SubTraffic{})
	if e != nil {
		t.Fatal(e)
	}
	old := map[string]bool{out[0].ResourceID: true, out[1].ResourceID: true}
	out[0].Password = "new-first"
	out[1].Password = "new-second"
	out, e = s.commitSourceRefresh(src, out, store.SubTraffic{})
	if e != nil {
		t.Fatal(e)
	}
	if old[out[0].ResourceID] || old[out[1].ResourceID] {
		t.Fatal("ambiguous accounts were silently rebound")
	}
}
func TestSourceReferenceBillingRouteAndRemoval(t *testing.T) {
	if !proBuild {
		t.Skip("subscription references require the professional edition")
	}
	s, c, src, ns := resourceFixture(t)
	if e := s.st.DB.Create(&store.Node{ID: "node", Name: "Node", DefaultCore: model.CoreXray}).Error; e != nil {
		t.Fatal(e)
	}
	s.st.DB.Create(&store.Core{ID: "core", NodeID: "node", Type: model.CoreXray, Version: "26.10.17"})
	in := store.Inbound{ID: "in", NodeID: "node", Tag: "entry", Core: model.CoreXray, Port: 20500, Enabled: true, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecNone})}
	if e := s.st.DB.Create(&in).Error; e != nil {
		t.Fatal(e)
	}
	s.st.DB.Model(&store.Core{}).Where("id = ?", "core").Update("no_user_traffic", true)
	if status, body := c.do("POST", "/sub-source-nodes/"+ns[0].ResourceID+"/references", map[string]any{"nodeId": "node", "tag": "unsupported-billing", "inboundId": "in"}); status != 400 {
		t.Fatal("unmetered relay was accepted", status, body)
	}
	var dangling int64
	s.st.DB.Model(&store.Outbound{}).Where("tag = ?", "unsupported-billing").Count(&dangling)
	if dangling != 0 {
		t.Fatal("failed relay binding left an outbound behind")
	}
	s.st.DB.Model(&store.Core{}).Where("id = ?", "core").Update("no_user_traffic", false)
	code, out := c.do("POST", "/sub-source-nodes/"+ns[0].ResourceID+"/references", map[string]any{"nodeId": "node", "tag": "provider", "inboundId": "in"})
	if code != 201 {
		t.Fatalf("bind: %d %v", code, out)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "upstream-password") {
		t.Fatal("API leaked credential")
	}
	spec, e := s.BuildSpec("node")
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, o := range spec.Outbounds {
		if o.Tag == "provider" {
			set, _ := o.ParseSettings()
			found = set.Password == "upstream-password-one" && o.Type == model.OutTrojan
		}
	}
	if !found {
		t.Fatal("source not resolved")
	}
	if code, out = c.do("DELETE", "/sub-sources/source", nil); code != 409 {
		t.Fatal("referenced source deleted", code, out)
	}
	if code, out = c.do("DELETE", "/sub-source-nodes/"+ns[0].ResourceID, nil); code != 409 {
		t.Fatal("referenced node deleted", code, out)
	}
	if _, e = s.commitSourceRefresh(src, ns[1:], store.SubTraffic{}); e != nil {
		t.Fatal(e)
	}
	spec, e = s.BuildSpec("node")
	if e != nil {
		t.Fatal(e)
	}
	blocked := false
	for _, r := range spec.Routes {
		if len(r.Match.Inbound) == 1 && r.Match.Inbound[0] == "entry" {
			blocked = r.Outbound == "block"
		}
	}
	if !blocked {
		t.Fatalf("removed credential fell through to direct: %+v", spec)
	}
	for _, o := range spec.Outbounds {
		if o.Tag == "provider" {
			t.Fatal("removed credentials still published")
		}
	}
	if code, out = c.do("DELETE", "/nodes/node/outbounds/"+outboundResourceID(t, s, ns[0].ResourceID), nil); code != 409 {
		t.Fatal("bound outbound deleted", code, out)
	}
}
func outboundResourceID(t *testing.T, s *Server, id string) string {
	t.Helper()
	var o store.Outbound
	if e := s.st.DB.First(&o, "source_node_id = ?", id).Error; e != nil {
		t.Fatal(e)
	}
	return o.ID
}
func TestBlockedSourcePathsSuspendGroupsAndDetours(t *testing.T) {
	spec := &model.NodeSpec{Outbounds: []model.Outbound{{Tag: "missing", Type: model.OutBlock}, {Tag: "detour", Type: model.OutSOCKS, Detour: "missing"}, {Tag: "group", Type: model.OutURLTest, Settings: mustJSON(model.OutboundSettings{Members: []string{"detour", "direct"}})}}, Routes: []model.RouteRule{{Outbound: "group"}, {Outbound: "detour"}, {Outbound: "missing"}, {Outbound: "direct"}}}
	collapseBlockedSourcePaths(spec)
	if len(spec.Outbounds) != 0 {
		t.Fatal("dangling reject aliases", spec.Outbounds)
	}
	for i, r := range spec.Routes {
		if i < 3 && r.Outbound != "block" {
			t.Fatal("route not blocked", r)
		}
	}
}

func TestSourceResourceBackupResealsCredentials(t *testing.T) {
	s, _, src, ns := resourceFixture(t)
	src.URL = s.st.Box.Seal("https://provider.example/?token=backup-secret")
	if e := s.st.DB.Save(src).Error; e != nil {
		t.Fatal(e)
	}
	var archive bytes.Buffer
	if e := writeArchive(&archive, s.st, s.cfg.DataDir, s.cfg.SecretKey, "archive-password"); e != nil {
		t.Fatal(e)
	}
	restored := newTestServer(t)
	box, _ := secret.New("different-panel-key")
	restored.st.Box = box
	arc, e := readArchive(bytes.NewReader(archive.Bytes()), "archive-password", box)
	if e != nil {
		t.Fatal(e)
	}
	if e = restoreArchive(restored.st, restored.cfg.DataDir, arc, box); e != nil {
		t.Fatal(e)
	}
	var row store.SubSourceNode
	if e = restored.st.DB.First(&row, "id = ?", ns[0].ResourceID).Error; e != nil {
		t.Fatal(e)
	}
	n, e := restored.openSourceNode(row)
	if e != nil || n.Password != "upstream-password-one" {
		t.Fatal("backup lost resource credentials", e)
	}
	if _, e = s.st.Box.Open(row.Payload); e == nil {
		t.Fatal("payload still encrypted under old key")
	}
	var after store.SubSource
	restored.st.DB.First(&after, "id = ?", src.ID)
	if box.MustOpen(after.URL) != "https://provider.example/?token=backup-secret" || !strings.HasPrefix(after.Cache, "enc:v1:") {
		t.Fatal("backup lost sealed source")
	}
}
