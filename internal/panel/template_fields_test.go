package panel

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestTemplateSelectiveFieldSync(t *testing.T) {
	s, c, id, opt := templateFixture(t)
	opt["planId"] = previewFixture(t, c, id, opt)["planId"]
	if code, out := c.do("POST", "/templates/"+id+"/apply", opt); code != 200 {
		t.Fatalf("apply: %d %v", code, out)
	}
	var in store.Inbound
	s.st.DB.Where("node_id = ? AND template_id = ?", "n1", id).First(&in)
	p := in.Protocol.V
	p.VisionUDP443 = true
	if code, out := c.do("PUT", "/inbounds/"+in.ID, map[string]any{"nodeId": "n1", "core": in.Core, "tag": in.Tag, "port": in.Port, "protocol": p, "transport": in.Transport.V,
		"security": map[string]any{"type": "reality", "realityDest": in.Security.V.RealityDest, "realityServerNames": in.Security.V.RealityServerNames}}); code != 200 {
		t.Fatalf("edit: %d %v", code, out)
	}
	s.st.DB.First(&in, "id = ?", in.ID)
	if !contains(in.OverriddenFields.V, "protocol.visionUDP443") || contains(in.OverriddenFields.V, "protocol") {
		t.Fatalf("coarse override: %v", in.OverriddenFields.V)
	}
	var tpl store.InboundTemplate
	s.st.DB.First(&tpl, "id = ?", id)
	tpl.Items.V[0].Protocol.TestSeed = []int{1, 2, 3, 4}
	tpl.Items.V[0].Security.RealityDest = "new.example.com:443"
	s.st.DB.Model(&tpl).Update("items", tpl.Items)
	url := "/templates/" + id + "/sync"
	code, diff := c.do("POST", url, map[string]any{"confirm": false})
	if code != 200 {
		t.Fatalf("diff: %d %v", code, diff)
	}
	var hash string
	for _, row := range diff["changes"].([]any) {
		change := row.(map[string]any)
		if change["inboundId"] == in.ID {
			hash = change["hash"].(string)
		}
	}
	if hash == "" {
		t.Fatal("missing change fingerprint")
	}
	code, out := c.do("POST", url, map[string]any{"confirm": true, "templateHash": diff["templateHash"], "expected": map[string]string{in.ID: hash}, "selections": map[string][]string{in.ID: {"protocol.testSeed"}}})
	if code != 200 {
		t.Fatalf("sync: %d %v", code, out)
	}
	s.st.DB.First(&in, "id = ?", in.ID)
	if !in.Protocol.V.VisionUDP443 || len(in.Protocol.V.TestSeed) != 4 || in.Security.V.RealityDest != "www.example.com:443" {
		t.Fatalf("selected sync changed other fields: %+v", in.Protocol.V)
	}
	var other store.Inbound
	s.st.DB.Where("node_id = ? AND template_id = ?", "n2", id).First(&other)
	if len(other.Protocol.V.TestSeed) != 0 {
		t.Fatal("unselected node changed")
	}
	if strings.Contains(fmt.Sprint(diff), s.toModelInbound(&in).Security.RealityPrivateKey) {
		t.Fatal("diff exposed a private key")
	}
	// An edit after preview invalidates the synchronization confirmation.
	code, _ = c.do("POST", url, map[string]any{"confirm": true, "expected": map[string]string{in.ID: hash}, "selections": map[string][]string{in.ID: {"security.realityDest"}}})
	if code != 409 {
		t.Fatal("stale sync accepted")
	}
}

func TestTemplateFieldOverridesAndRedaction(t *testing.T) {
	old := model.ProtocolSpec{Type: model.ProtoVLESS, Decryption: "old-secret"}
	want := old
	want.Decryption, want.VisionUDP443 = "new-secret", true
	var merged model.ProtocolSpec
	fields, kept, details := mergeTemplateGroup("protocol", old, want, &merged, map[string]bool{"protocol": true}, nil)
	if len(fields) != 0 || len(kept) != 2 || merged.Decryption != "old-secret" {
		t.Fatal("legacy group override was lost")
	}
	if strings.Contains(fmt.Sprint(details), "secret") {
		t.Fatal("secret appears in field values")
	}
	fields, _, _ = mergeTemplateGroup("protocol", old, want, &merged, nil, map[string]bool{"protocol.visionUDP443": true})
	if len(fields) != 1 || !merged.VisionUDP443 || merged.Decryption != "old-secret" {
		t.Fatal("partial field selection failed")
	}
}

func TestTemplateNodeVariablesStayQuietOnSync(t *testing.T) {
	s, c, id, opt := templateFixture(t)
	for _, node := range []string{"n1", "n2"} {
		s.st.DB.Model(&store.Node{}).Where("id = ?", node).Update("domain", node+".example.com")
	}
	var tpl store.InboundTemplate
	s.st.DB.First(&tpl, "id = ?", id)
	tpl.Items.V[0].Security.RealityDest = "{node.domain}:443"
	tpl.Items.V[0].Security.RealityServerNames = []string{"{node.domain}"}
	s.st.DB.Model(&tpl).Update("items", tpl.Items)
	opt["planId"] = previewFixture(t, c, id, opt)["planId"]
	if code, out := c.do("POST", "/templates/"+id+"/apply", opt); code != 200 {
		t.Fatalf("apply: %d %v", code, out)
	}
	if code, out := c.do("POST", "/templates/"+id+"/sync", map[string]bool{"confirm": false}); code != 200 || len(out["changes"].([]any)) != 0 {
		t.Fatalf("variables create spurious sync changes: %d %v", code, out)
	}
	s.st.DB.First(&tpl, "id = ?", id)
	if tpl.Items.V[0].Security.RealityServerNames[0] != "{node.domain}" {
		t.Fatal("node expansion mutated the template")
	}
}
