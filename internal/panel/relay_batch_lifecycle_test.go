package panel

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestRelayBatchGroupLifecycle(t *testing.T) {
	for _, mode := range []string{"l4", "tunnel"} {
		t.Run(mode, func(t *testing.T) {
			s, c := relayBatchFixture(t)
			u := relayTestUser(t, s)
			if code, out := c.do("PUT", "/inbounds/in/publication", map[string]any{"relayOnly": true}); code != 200 {
				t.Fatal(code, out)
			}
			created := createRelayBatchTest(t, c, previewRelayBatchTest(t, c, relayBatchRequestTest(mode, true)))
			batchID := created["batchId"].(string)
			path := "/inbounds/in/relay-batches/" + batchID
			// A second batch can share every physical node but owns separate listeners.
			other := createRelayBatchTest(t, c, previewRelayBatchTest(t, c, relayBatchRequestTest(mode, true)))
			if code, out := c.do("GET", path, nil); code != 200 || len(out["chains"].([]any)) != 2 {
				t.Fatal(code, out)
			}
			if code, out := c.do("PUT", path+"/state", map[string]any{"enabled": false}); code != 200 || out["state"] != "disabled" {
				t.Fatal(code, out)
			}
			var disabled int64
			s.st.DB.Model(&store.Chain{}).Where("batch_id = ? AND disabled = ?", batchID, true).Count(&disabled)
			if disabled != 2 {
				t.Fatal("batch partly disabled", disabled)
			}
			subs, err := s.subNodes(&u)
			if err != nil || len(subs) != 2 {
				t.Fatal("other batch was disabled", subs, err)
			}
			var audits int64
			s.st.DB.Model(&store.AuditLog{}).Where("action = ? AND target = ?", "relay.batch.state", batchID).Count(&audits)
			if code, out := c.do("PUT", path+"/state", map[string]any{"enabled": false}); code != 200 {
				t.Fatal(code, out)
			}
			var repeated int64
			s.st.DB.Model(&store.AuditLog{}).Where("action = ? AND target = ?", "relay.batch.state", batchID).Count(&repeated)
			if repeated != audits {
				t.Fatal("repeated state change duplicated audit")
			}
			if code, out := c.do("PUT", path+"/state", map[string]any{"enabled": true}); code != 200 {
				t.Fatal(code, out)
			}
			subs, err = s.subNodes(&u)
			if err != nil || len(subs) != 4 {
				t.Fatal(subs, err)
			}
			// A separately disabled member is represented as mixed, without implicitly
			// enabling it on reads or metadata updates.
			first := created["chains"].([]any)[0].(map[string]any)["id"].(string)
			if code, out := c.do("PUT", "/inbounds/in/relays/"+first+"/state", map[string]any{"enabled": false}); code != 200 {
				t.Fatal(code, out)
			}
			if code, out := c.do("GET", path, nil); code != 200 || out["state"] != "mixed" {
				t.Fatal(code, out)
			}
			// Removing a member independently leaves a usable one-member group.
			if code, out := c.do("DELETE", "/inbounds/in/relays/"+first, nil); code != 204 {
				t.Fatal(code, out)
			}
			if code, out := c.do("GET", path, nil); code != 200 || len(out["chains"].([]any)) != 1 {
				t.Fatal(code, out)
			}
			if code, out := c.do("DELETE", path, nil); code != 204 {
				t.Fatal(code, out)
			}
			if code, out := c.do("GET", path, nil); code != 404 {
				t.Fatal("deleted group remains", code, out)
			}
			for _, raw := range created["chains"].([]any) {
				id := raw.(map[string]any)["id"].(string)
				var count int64
				s.st.DB.Model(&store.Endpoint{}).Where("relay_chain_id = ?", id).Count(&count)
				if count != 0 {
					t.Fatal("orphaned endpoint", id, count)
				}
			}
			if code, out := c.do("GET", "/inbounds/in/relay-batches/"+other["batchId"].(string), nil); code != 200 || len(out["chains"].([]any)) != 2 {
				t.Fatal("shared-node batch was removed", code, out)
			}
			subs, err = s.subNodes(&u)
			if err != nil || len(subs) != 2 {
				t.Fatal(subs, err)
			}
			for _, node := range []string{"relay", "relay2", "middle", "land"} {
				if _, err := s.BuildSpec(node); err != nil {
					t.Fatal("remaining specs invalid", node, err)
				}
			}
			if code, out := c.do("DELETE", "/inbounds/in/relay-batches/"+other["batchId"].(string), nil); code != 204 {
				t.Fatal(code, out)
			}
			subs, err = s.subNodes(&u)
			if err != nil || len(subs) != 0 {
				t.Fatal("deleting last group restored direct access", subs, err)
			}
		})
	}
}

func TestRelayBatchGroupFailureRollsBack(t *testing.T) {
	for _, method := range []string{"PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			s, c := relayBatchFixture(t)
			created := createRelayBatchTest(t, c, previewRelayBatchTest(t, c, relayBatchRequestTest("tunnel", true)))
			batchID := created["batchId"].(string)
			path := "/inbounds/in/relay-batches/" + batchID
			for _, op := range []string{"GET", "PUT", "DELETE"} {
				p := "/inbounds/wrong/relay-batches/" + batchID
				if op == "PUT" {
					p += "/state"
				}
				if code, out := c.do(op, p, map[string]any{"enabled": false}); code != 404 {
					t.Fatal("cross-inbound group access", op, code, out)
				}
			}
			const callback = "test:reject_relay_batch_audit"
			if err := s.st.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "audit_logs" {
					tx.AddError(errors.New("audit unavailable"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			p := path
			if method == "PUT" {
				p += "/state"
			}
			code, _ := c.do(method, p, map[string]any{"enabled": false})
			s.st.DB.Callback().Create().Remove(callback)
			if code != 500 {
				t.Fatal("expected transactional failure", code)
			}
			var chains []store.Chain
			s.st.DB.Where("batch_id = ?", batchID).Find(&chains)
			if len(chains) != 2 || chains[0].Disabled || chains[1].Disabled {
				t.Fatal("partial batch mutation", chains)
			}
			for _, ch := range chains {
				var count int64
				s.st.DB.Model(&store.Endpoint{}).Where("relay_chain_id = ?", ch.ID).Count(&count)
				if count != 1 {
					t.Fatal("endpoint cleanup not rolled back", count)
				}
			}
			if code, out := c.do("PUT", path+"/state", map[string]any{"enabled": false}); code != 200 {
				t.Fatal("could not retry", code, out)
			}
		})
	}
}

func TestRelayBatchGroupInvalidMember(t *testing.T) {
	s, c := relayBatchFixture(t)
	created := createRelayBatchTest(t, c, previewRelayBatchTest(t, c, relayBatchRequestTest("l4", false)))
	path := fmt.Sprint("/inbounds/in/relay-batches/", created["batchId"])
	if code, _ := c.do("PUT", path+"/state", map[string]any{}); code != 400 {
		t.Fatal(code)
	}
	var ch store.Chain
	s.st.DB.Where("batch_id = ?", created["batchId"]).First(&ch)
	hops := ch.Hops.V
	hops[len(hops)-1].InboundID = "other"
	s.st.DB.Model(&ch).Update("hops", store.NewJSON(hops))
	if code, out := c.do("DELETE", path, nil); code != 404 {
		t.Fatal("partly owned group accepted", code, out)
	}
	var count int64
	s.st.DB.Model(&store.Chain{}).Where("batch_id = ?", created["batchId"]).Count(&count)
	if count != 2 {
		t.Fatal("partial group removed", count)
	}
}

func TestRelayBatchGroupKeepsRemainingSourceGuards(t *testing.T) {
	s, c := relayBatchFixture(t)
	for id, ip := range map[string]string{"relay": "198.51.100.1", "relay2": "198.51.100.2", "land": "198.51.100.3"} {
		if err := s.st.DB.Model(&store.Node{}).Where("id = ?", id).Updates(map[string]any{"public_ipv4": ip, "agent_features": store.NewJSON([]string{model.RelayAccessFeature})}).Error; err != nil {
			t.Fatal(err)
		}
	}
	a := createRelayBatchTest(t, c, previewRelayBatchTest(t, c, relayBatchRequestTest("l4", false)))
	b := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay2"}}}))
	if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true}); code != 200 {
		t.Fatal(code, out)
	}
	if code, out := c.do("DELETE", fmt.Sprint("/inbounds/in/relay-batches/", a["batchId"]), nil); code != 204 {
		t.Fatal(code, out)
	}
	spec, err := s.BuildSpec("land")
	if err != nil || len(spec.AccessRules) != 1 || len(spec.AccessRules[0].Sources) != 1 || spec.AccessRules[0].Sources[0] != "198.51.100.2/32" {
		t.Fatal("remaining source guard changed", spec, err)
	}
	if code, out := c.do("DELETE", "/inbounds/in/relays/"+b, nil); code != 204 {
		t.Fatal(code, out)
	}
	spec, err = s.BuildSpec("land")
	if err != nil || len(spec.AccessRules) != 1 || len(spec.AccessRules[0].Sources) != 0 {
		t.Fatal("last deletion lifted source guard", spec, err)
	}
}
