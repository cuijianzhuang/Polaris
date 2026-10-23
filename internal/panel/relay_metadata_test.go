package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestRelayMetadataKeepsDeploymentAndUpdatesPublication(t *testing.T) {
	for _, mode := range []string{"l4", "tunnel"} {
		t.Run(mode, func(t *testing.T) {
			s, c := relayFixture(t)
			u := relayTestUser(t, s)
			if code, out := c.do("PUT", "/inbounds/in/publication", map[string]any{"relayOnly": true}); code != 200 {
				t.Fatal(code, out)
			}
			id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"mode": mode, "relays": []any{map[string]any{"nodeId": "relay"}}}))
			var before store.Chain
			if err := s.st.DB.First(&before, "id = ?", id).Error; err != nil {
				t.Fatal(err)
			}
			var ep store.Endpoint
			s.st.DB.First(&ep, "relay_chain_id = ?", id)
			specs := map[string]*model.NodeSpec{}
			for _, node := range []string{"relay", "land"} {
				spec, err := s.BuildSpec(node)
				if err != nil {
					t.Fatal(err)
				}
				specs[node] = spec
			}
			code, out := c.do("PUT", "/inbounds/in/relays/"+id+"/metadata", map[string]any{"name": "上海 → 香港", "remark": "运营备注", "shareAddress": "SHARE.EXAMPLE.COM.", "sharePort": 8443})
			if code != 200 || out["shareAddress"] != "share.example.com" || out["sharePort"] != float64(8443) || out["shareName"] != "上海 → 香港" {
				t.Fatal(code, out)
			}
			var after store.Chain
			s.st.DB.First(&after, "id = ?", id)
			if !reflect.DeepEqual(before.Hops, after.Hops) || before.TunnelAuth != after.TunnelAuth || !reflect.DeepEqual(before.TargetRevs, after.TargetRevs) || before.BatchID != after.BatchID || before.Disabled != after.Disabled {
				t.Fatal("metadata changed path, revision or credentials")
			}
			var epAfter store.Endpoint
			s.st.DB.First(&epAfter, "relay_chain_id = ?", id)
			if ep.ID != epAfter.ID || epAfter.Address != "share.example.com" || epAfter.Port != 8443 || epAfter.ShareName != after.Name {
				t.Fatal("endpoint recreated or publication not updated", epAfter)
			}
			for node, before := range specs {
				after, err := s.BuildSpec(node)
				if err != nil || !sameSpec(before, after) {
					t.Fatal("metadata changed runtime spec", node, err)
				}
			}
			subs, err := s.subNodes(&u)
			if err != nil || len(subs) != 1 || subs[0].Server != "share.example.com" || subs[0].Port != 8443 || subs[0].Name != "上海 → 香港" || subs[0].UUID != u.UUID {
				t.Fatal("subscription mismatch", subs, err)
			}
			if code, out := c.do("GET", "/users/"+u.ID+"/links", nil); code != 200 || !strings.Contains(string(mustRelayJSON(t, out)), "share.example.com") {
				t.Fatal("share links not updated", code, out)
			}
			// Omitted fields are retained, even when editing a disabled path.
			if code, out := c.do("PUT", "/inbounds/in/relays/"+id+"/state", map[string]any{"enabled": false}); code != 200 {
				t.Fatal(code, out)
			}
			if code, out := c.do("PUT", "/inbounds/in/relays/"+id+"/metadata", map[string]any{"remark": "停用期间修改"}); code != 200 || out["shareName"] != "上海 → 香港" {
				t.Fatal(code, out)
			}
			subs, err = s.subNodes(&u)
			if err != nil || len(subs) != 0 {
				t.Fatal("metadata re-enabled a disabled path", subs, err)
			}
			// Empty address, zero port and empty share name restore inherited values.
			if code, out := c.do("PUT", "/inbounds/in/relays/"+id+"/metadata", map[string]any{"shareAddress": "", "sharePort": 0, "shareName": ""}); code != 200 {
				t.Fatal(code, out)
			}
			if code, out := c.do("PUT", "/inbounds/in/relays/"+id+"/state", map[string]any{"enabled": true}); code != 200 {
				t.Fatal(code, out)
			}
			subs, err = s.subNodes(&u)
			if err != nil || len(subs) != 1 || subs[0].Server != "relay.example.com" || subs[0].Port != before.Hops.V[0].Port || subs[0].Name == "上海 → 香港" {
				t.Fatal("publication reset failed", subs, err)
			}
		})
	}
}

func mustRelayJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestRelayMetadataValidationAndRollback(t *testing.T) {
	s, c := relayFixture(t)
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	path := "/inbounds/in/relays/" + id + "/metadata"
	for _, req := range []map[string]any{
		{}, {"name": " "}, {"name": strings.Repeat("字", 65)}, {"name": "name\nline"},
		{"remark": strings.Repeat("x", 256)}, {"sharePort": -1}, {"sharePort": 65536},
		{"shareAddress": "https://example.com"}, {"shareAddress": "example.com:443"},
		{"shareAddress": "example.com/path"}, {"shareAddress": "user@example.com"}, {"shareAddress": "-bad.example"},
		{"shareAddress": "[example.com]"}, {"shareName": "bad\nname"}, {"shareAddress": "[fe80::1%eth0]"},
	} {
		if code, out := c.do("PUT", path, req); code != 400 {
			t.Fatal(req, code, out)
		}
	}
	if code, out := c.do("PUT", "/inbounds/other/relays/"+id+"/metadata", map[string]any{"name": "other"}); code != 404 {
		t.Fatal("wrong inbound accepted", code, out)
	}
	var before store.Chain
	s.st.DB.First(&before, "id = ?", id)
	if err := s.st.DB.Create(&store.Chain{ID: "other", Name: "taken", Mode: "l4", Managed: false}).Error; err != nil {
		t.Fatal(err)
	}
	if code, out := c.do("PUT", path, map[string]any{"name": "taken", "shareAddress": "not-saved.example"}); code != 409 {
		t.Fatal("duplicate name", code, out)
	}
	// A failure after updating the chain must roll back both its name and its
	// endpoint. The audit belongs to that same transaction.
	const callback = "test:reject_relay_metadata_audit"
	if err := s.st.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "audit_logs" {
			tx.AddError(errors.New("audit unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	code, _ := c.do("PUT", path, map[string]any{"name": "rolled-back", "shareAddress": "not-saved.example", "sharePort": 8443})
	s.st.DB.Callback().Create().Remove(callback)
	if code != 500 {
		t.Fatal("audit failure did not fail request", code)
	}
	var after store.Chain
	s.st.DB.First(&after, "id = ?", id)
	var ep store.Endpoint
	s.st.DB.First(&ep, "relay_chain_id = ?", id)
	if after.Name != before.Name || ep.Address != "" || ep.Port != 0 || ep.ShareName != "" {
		t.Fatal("partial metadata persisted", after, ep)
	}
	if code, out := c.do("PUT", "/inbounds/in/relays/other/metadata", map[string]any{"name": "manual"}); code != 404 {
		t.Fatal("unmanaged chain accepted", code, out)
	}
}

func TestRelayShareAddress(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""}, {"  EXAMPLE.COM. ", "example.com"}, {"[2001:db8::1]", "2001:db8::1"},
		{"203.0.113.1", "203.0.113.1"}, {"例子.测试", "xn--fsqu00a.xn--0zwm56d"},
	} {
		got, err := relayShareAddress(tc.in)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
}

func TestRelayMetadataParallelEntryIsolation(t *testing.T) {
	s, c := relayBatchFixture(t)
	u := relayTestUser(t, s)
	if code, out := c.do("PUT", "/inbounds/in/publication", map[string]any{"relayOnly": true}); code != 200 {
		t.Fatal(code, out)
	}
	created := createRelayBatchTest(t, c, previewRelayBatchTest(t, c, relayBatchRequestTest("tunnel", true)))
	first := created["chains"].([]any)[0].(map[string]any)["id"].(string)
	if code, out := c.do("PUT", "/inbounds/in/relays/"+first+"/metadata", map[string]any{"shareAddress": "one.example.com", "sharePort": 443, "shareName": "first"}); code != 200 {
		t.Fatal(code, out)
	}
	subs, err := s.subNodes(&u)
	if err != nil || len(subs) != 2 {
		t.Fatal(subs, err)
	}
	seen := map[string]bool{}
	for _, n := range subs {
		seen[n.Server] = true
	}
	if !seen["one.example.com"] || !seen["relay2.example.com"] {
		t.Fatal("other entry publication changed", subs)
	}
}

func TestRelayMetadataBackupRestore(t *testing.T) {
	s, c := relayFixture(t)
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	if code, out := c.do("PUT", "/inbounds/in/relays/"+id+"/metadata", map[string]any{"shareName": "exported relay", "shareAddress": "exported.example", "sharePort": 8443}); code != 200 {
		t.Fatal(code, out)
	}
	var archive bytes.Buffer
	if err := writeArchive(&archive, s.st, s.cfg.DataDir, s.cfg.SecretKey, ""); err != nil {
		t.Fatal(err)
	}
	arc, err := readArchive(bytes.NewReader(archive.Bytes()), "", s.st.Box)
	if err != nil {
		t.Fatal(err)
	}
	restored := newTestServer(t)
	if err := restoreArchive(restored.st, restored.cfg.DataDir, arc, restored.st.Box); err != nil {
		t.Fatal(err)
	}
	var ep store.Endpoint
	if err := restored.st.DB.First(&ep, "relay_chain_id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if ep.ShareName != "exported relay" || ep.Address != "exported.example" || ep.Port != 8443 {
		t.Fatal("backup lost publication metadata", ep)
	}
}
