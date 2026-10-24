package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func applyRelayChange(t *testing.T, c *apiClient, method, path, etag string, body any) (int, map[string]any) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.tok)
	req.Header.Set("Content-Type", "application/json")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func previewRelayChangeTest(t *testing.T, c *apiClient, path, action string) map[string]any {
	t.Helper()
	code, out := c.do("POST", path+"/change-preview", map[string]any{"action": action})
	if code != 200 {
		t.Fatal(code, out)
	}
	if len(out["etag"].(string)) != 66 {
		t.Fatal("missing strong ETag", out)
	}
	return out
}

func TestRelayChangeBatchPreviewAndApply(t *testing.T) {
	s, c := relayBatchFixture(t)
	if code, out := c.do("PUT", "/inbounds/in/publication", map[string]any{"relayOnly": true}); code != 200 {
		t.Fatal(code, out)
	}
	created := createRelayBatchTest(t, c, previewRelayBatchTest(t, c, relayBatchRequestTest("tunnel", true)))
	other := createRelayBatchTest(t, c, previewRelayBatchTest(t, c, relayBatchRequestTest("tunnel", true)))
	path := "/inbounds/in/relay-batches/" + created["batchId"].(string)
	var before, after int64
	s.st.DB.Model(&store.AuditLog{}).Count(&before)
	preview := previewRelayChangeTest(t, c, path, "delete")
	s.st.DB.Model(&store.AuditLog{}).Count(&after)
	if before != after {
		t.Fatal("read-only preview wrote audit")
	}
	if preview["blocked"] != false || preview["changedPaths"] != float64(2) || preview["remainingRelayPaths"] != float64(2) || preview["remainingEnabledRelayPaths"] != float64(2) || len(preview["endpointIds"].([]any)) != 2 {
		t.Fatal(preview)
	}
	for _, node := range preview["nodes"].([]any) {
		n := node.(map[string]any)
		if n["otherPaths"].(float64) == 0 {
			t.Fatal("shared nodes lost remaining references", n)
		}
	}
	var chains []store.Chain
	s.st.DB.Where("batch_id = ?", created["batchId"]).Find(&chains)
	b, _ := json.Marshal(preview)
	for _, ch := range chains {
		raw, _ := s.st.Box.Open(ch.TunnelAuth)
		for _, secret := range []string{raw, ch.TunnelAuth} {
			if secret != "" && strings.Contains(string(b), secret) {
				t.Fatal("preview disclosed tunnel credentials")
			}
		}
	}
	if code, out := applyRelayChange(t, c, "DELETE", path, preview["etag"].(string), nil); code != 204 {
		t.Fatal(code, out)
	}
	var count int64
	s.st.DB.Model(&store.Chain{}).Where("batch_id = ?", other["batchId"]).Count(&count)
	if count != 2 {
		t.Fatal("other batch was removed")
	}
	if code, out := c.do("GET", "/inbounds/in/relay-batches/"+other["batchId"].(string), nil); code != 200 || len(out["chains"].([]any)) != 2 {
		t.Fatal(code, out)
	}
}

func TestRelayChangePreviewStaleAndActionBinding(t *testing.T) {
	s, c := relayFixture(t)
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	path := "/inbounds/in/relays/" + id
	preview := previewRelayChangeTest(t, c, path, "delete")
	// A delete review cannot authorize a state change.
	if code, _ := applyRelayChange(t, c, "PUT", path+"/state", preview["etag"].(string), map[string]any{"enabled": false}); code != 412 {
		t.Fatalf("wrong action %d", code)
	}
	if code, out := c.do("PUT", path+"/metadata", map[string]any{"shareAddress": "new.example.com"}); code != 200 {
		t.Fatal(code, out)
	}
	if code, _ := applyRelayChange(t, c, "DELETE", path, preview["etag"].(string), nil); code != 412 {
		t.Fatalf("stale preview %d", code)
	}
	var count int64
	s.st.DB.Model(&store.Chain{}).Where("id = ?", id).Count(&count)
	if count != 1 {
		t.Fatal("stale precondition mutated chain")
	}
	for _, tag := range []string{"*", "W/" + preview["etag"].(string), "unquoted"} {
		if code, _ := applyRelayChange(t, c, "DELETE", path, tag, nil); code != 400 {
			t.Fatalf("invalid condition %q %d", tag, code)
		}
	}
	// Legacy calls remain valid, but reference checks are still performed.
	if code, out := c.do("DELETE", path, nil); code != 204 {
		t.Fatal(code, out)
	}
}

func TestRelayChangePreviewIgnoresMeasurementsAndNoop(t *testing.T) {
	s, c := relayFixture(t)
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	path := "/inbounds/in/relays/" + id
	preview := previewRelayChangeTest(t, c, path, "disable")
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "relay").Updates(map[string]any{"last_seen": time.Now(), "net_rx": 12345, "system": store.NewJSON(store.SystemInfo{CPU: 72})}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Model(&store.Chain{}).Where("id = ?", id).Updates(map[string]any{"probe": store.NewJSON([]store.HopProbe{{}}), "probed_at": time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	second := previewRelayChangeTest(t, c, path, "disable")
	if preview["etag"] != second["etag"] {
		t.Fatal("routine measurement invalidated configuration review")
	}
	if code, out := applyRelayChange(t, c, "PUT", path+"/state", preview["etag"].(string), map[string]any{"enabled": false}); code != 200 {
		t.Fatal(code, out)
	}
	noop := previewRelayChangeTest(t, c, path, "disable")
	if noop["changedPaths"] != float64(0) {
		t.Fatal("no-op counted as a change")
	}
	var before, after int64
	s.st.DB.Model(&store.AuditLog{}).Where("action = ?", "relay.state").Count(&before)
	if code, out := applyRelayChange(t, c, "PUT", path+"/state", noop["etag"].(string), map[string]any{"enabled": false}); code != 200 {
		t.Fatal(code, out)
	}
	s.st.DB.Model(&store.AuditLog{}).Where("action = ?", "relay.state").Count(&after)
	if before != after {
		t.Fatal("repeated state operation duplicated audit")
	}
}

func proxyChangeFixture(t *testing.T) (*Server, *apiClient, string, store.Outbound, store.User) {
	t.Helper()
	s, c := relayFixture(t)
	if err := s.st.DB.Create(&store.Core{ID: "relay-core", NodeID: "relay", Type: model.CoreXray, Version: "26.3.27"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.Inbound{ID: "proxy-in", NodeID: "relay", Tag: "proxy-in", Port: 12345, Enabled: true, Core: model.CoreXray, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecNone})}).Error; err != nil {
		t.Fatal(err)
	}
	code, out := c.do("POST", "/chains", chainReq{Name: "proxy", Mode: "proxy", Hops: []store.ChainHop{{NodeID: "relay", InboundID: "proxy-in"}, {NodeID: "land", InboundID: "in"}}})
	if code != 201 {
		t.Fatal(code, out)
	}
	id := out["id"].(string)
	var o store.Outbound
	if err := s.st.DB.First(&o, "chain_id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	var user store.User
	if err := s.st.DB.First(&user, "id = ?", o.ServiceUserID).Error; err != nil {
		t.Fatal(err)
	}
	return s, c, id, o, user
}

func TestRelayChangeExternalReferencesProtectDeletion(t *testing.T) {
	for _, kind := range []string{"balancer", "fallback", "detour", "inbound", "routing", "global-routing", "service-outbound", "service-chain"} {
		t.Run(kind, func(t *testing.T) {
			s, c, id, owned, svc := proxyChangeFixture(t)
			var err error
			switch kind {
			case "balancer":
				settings, _ := json.Marshal(model.OutboundSettings{Members: []string{owned.Tag}})
				err = s.st.DB.Create(&store.Outbound{ID: "external", NodeID: owned.NodeID, Tag: "external", Type: model.OutBalancer, Settings: settings}).Error
			case "fallback":
				settings, _ := json.Marshal(model.OutboundSettings{Members: []string{"direct"}, Fallback: owned.Tag})
				err = s.st.DB.Create(&store.Outbound{ID: "external", NodeID: owned.NodeID, Tag: "external", Type: model.OutBalancer, Settings: settings}).Error
			case "detour":
				err = s.st.DB.Create(&store.Outbound{ID: "external", NodeID: owned.NodeID, Tag: "external", Type: model.OutSOCKS, Detour: owned.Tag}).Error
			case "inbound":
				err = s.st.DB.Model(&store.Inbound{}).Where("id = ?", "proxy-in").Update("outbound_tag", owned.Tag).Error
			case "routing", "global-routing":
				node := owned.NodeID
				if kind == "global-routing" {
					node = model.GlobalNodeID
				}
				err = s.st.DB.Create(&store.RoutingRule{ID: "external", NodeID: node, OutboundTag: owned.Tag}).Error
			case "service-outbound":
				err = s.st.DB.Create(&store.Outbound{ID: "external", NodeID: "middle", Tag: "external", Type: model.OutNodeRef, ServiceUserID: svc.ID}).Error
			case "service-chain":
				err = s.st.DB.Create(&store.Chain{ID: "external", Name: "external", Mode: "proxy", ServiceUserID: svc.ID, Hops: store.NewJSON([]store.ChainHop{{NodeID: "middle"}, {NodeID: "land", InboundID: "in"}})}).Error
			}
			if err != nil {
				t.Fatal(err)
			}
			path := "/chains/" + id
			preview := previewRelayChangeTest(t, c, path, "delete")
			if preview["blocked"] != true || len(preview["references"].([]any)) == 0 {
				t.Fatal("external reference not reported", preview)
			}
			body, _ := json.Marshal(preview)
			for _, secret := range []string{svc.UUID, svc.Password, svc.SubToken} {
				if secret != "" && strings.Contains(string(body), secret) {
					t.Fatal("service credential exposed")
				}
			}
			for _, tag := range []string{"", preview["etag"].(string)} {
				if code, out := applyRelayChange(t, c, "DELETE", path, tag, nil); code != 409 {
					t.Fatal("referenced deletion allowed", code, out)
				}
			}
			if code, out := c.do("PUT", path, chainReq{Name: "proxy-new", Mode: "proxy", Hops: []store.ChainHop{{NodeID: "relay", InboundID: "proxy-in"}, {NodeID: "land", InboundID: "in"}}}); code != 409 {
				t.Fatal("referenced resources rebuilt", code, out)
			}
			var count int64
			s.st.DB.Model(&store.Outbound{}).Where("id = ?", owned.ID).Count(&count)
			if count != 1 {
				t.Fatal("failed operation removed outbound")
			}
			var retained store.User
			if err := s.st.DB.First(&retained, "id = ?", svc.ID).Error; err != nil || retained.UUID != svc.UUID {
				t.Fatal("failed operation changed service identity")
			}
		})
	}
}

func TestRelayChangeOwnedReferencesDoNotBlock(t *testing.T) {
	s, c, id, owned, svc := proxyChangeFixture(t)
	path := "/chains/" + id
	preview := previewRelayChangeTest(t, c, path, "delete")
	if preview["blocked"] != false || len(preview["outboundIds"].([]any)) != 1 || len(preview["routingRuleIds"].([]any)) != 1 || len(preview["serviceUserIds"].([]any)) != 1 {
		t.Fatal(preview)
	}
	if code, out := applyRelayChange(t, c, "DELETE", path, preview["etag"].(string), nil); code != 204 {
		t.Fatal(code, out)
	}
	var n int64
	s.st.DB.Model(&store.Outbound{}).Where("id = ?", owned.ID).Count(&n)
	if n != 0 {
		t.Fatal("owned outbound retained")
	}
	s.st.DB.Model(&store.User{}).Where("id = ?", svc.ID).Count(&n)
	if n != 0 {
		t.Fatal("unreferenced service account retained")
	}
}

func TestRelayChangePreviewScopeAndLastEntry(t *testing.T) {
	s, c := relayBatchFixture(t)
	if code, out := c.do("PUT", "/inbounds/in/publication", map[string]any{"relayOnly": true}); code != 200 {
		t.Fatal(code, out)
	}
	created := createRelayBatchTest(t, c, previewRelayBatchTest(t, c, relayBatchRequestTest("l4", false)))
	path := "/inbounds/in/relay-batches/" + created["batchId"].(string)
	preview := previewRelayChangeTest(t, c, path, "disable")
	if preview["remainingEnabledRelayPaths"] != float64(0) || preview["relayOnly"] != true || len(preview["warnings"].([]any)) == 0 {
		t.Fatal("last-entry impact hidden", preview)
	}
	if code, out := applyRelayChange(t, c, "PUT", path+"/state", preview["etag"].(string), map[string]any{"enabled": false}); code != 200 {
		t.Fatal(code, out)
	}
	var in store.Inbound
	s.st.DB.First(&in, "id = ?", "in")
	if !in.RelayOnly {
		t.Fatal("preview/state disabled relay-only policy")
	}
	if code, _ := c.do("POST", strings.Replace(path, "/inbounds/in/", "/inbounds/other/", 1)+"/change-preview", map[string]any{"action": "delete"}); code != 404 {
		t.Fatal("wrong inbound accepted")
	}
	if code, _ := c.do("POST", path+"/change-preview", map[string]any{"action": "rename"}); code != 400 {
		t.Fatal("unknown operation accepted")
	}
	if code, _ := (&apiClient{t: t, base: c.base}).do("POST", path+"/change-preview", map[string]any{"action": "delete"}); code != 401 {
		t.Fatal("anonymous preview accepted")
	}
}
