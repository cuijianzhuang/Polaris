package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestProxyChainRejectsSingboxXHTTPNextHop(t *testing.T) {
	s, c, id, _, _ := proxyChangeFixture(t)
	s.st.DB.Model(&store.Core{}).Where("id = ?", "relay-core").Updates(map[string]any{"type": model.CoreSingbox, "version": "1.13.5"})
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "proxy-in").Update("core", model.CoreSingbox)
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("transport", store.NewJSON(model.TransportSpec{Type: model.TransXHTTP, Path: "/xhttp"}))
	var ch store.Chain
	s.st.DB.First(&ch, "id = ?", id)
	if code, out := c.do("PUT", "/chains/"+id, chainReq{Name: ch.Name, Mode: ch.Mode, Hops: ch.Hops.V}); code != 400 || !strings.Contains(strings.ToLower(out["error"].(string)), "xhttp") {
		t.Fatal("sing-box to XHTTP chain accepted", code, out)
	}
	if _, err := s.BuildSpec("relay"); err == nil || !strings.Contains(err.Error(), "xhttp") {
		t.Fatal("existing XHTTP node_ref degraded to TCP", err)
	}
	if code, rows := c.doList("GET", "/chains"); code != 200 || len(rows) != 1 || len(rows[0].(map[string]any)["configurationIssues"].([]any)) == 0 {
		t.Fatal("existing incompatible chain has no visible reason", code, rows)
	}
	for _, body := range []map[string]any{
		{"tag": "manual-ref", "type": "node_ref", "nodeRefInboundId": "in"},
		{"tag": "manual-vless", "type": "vless", "settings": map[string]any{"server": "example.com", "port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "transport": map[string]any{"type": "xhttp", "path": "/xhttp"}}},
	} {
		if code, out := c.do("POST", "/nodes/relay/outbounds", body); code != 400 || !strings.Contains(out["error"].(string), "xhttp") {
			t.Fatal("manual outbound bypassed transport validation", code, out)
		}
	}
	// The same target remains valid for an Xray client; this is a source-core
	// restriction, not a blanket ban on XHTTP paths.
	s.st.DB.Model(&store.Core{}).Where("id = ?", "relay-core").Updates(map[string]any{"type": model.CoreXray, "version": "26.3.27"})
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "proxy-in").Update("core", model.CoreXray)
	if code, out := c.do("PUT", "/chains/"+id, chainReq{Name: ch.Name, Mode: ch.Mode, Hops: ch.Hops.V}); code != 200 {
		t.Fatal("Xray XHTTP path was incorrectly rejected", code, out)
	}
}
