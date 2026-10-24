package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func TestSnellHTTPVersionFilteringAndBuiltinConversion(t *testing.T) {
	s := newTestServer(t)
	var external atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		external.Add(1)
		http.Error(w, "must not convert Snell through link callback", 500)
	}))
	defer backend.Close()
	node := sub.Node{Name: "snell", Server: "127.0.0.1", Port: 1234, Protocol: model.ProtoSnell, SnellVersion: 4, SnellPSK: "server-secret", SnellUserKey: "user-secret"}
	for _, tc := range []struct {
		ua   string
		want bool
	}{{"sing-box/1.14.4", true}, {"sing-box/1.13.5", false}, {"SFA/1.13.5", false}, {"Hiddify/2.0", false}, {"Mozilla/5.0", true}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://panel.test/sub/u?format=singbox", nil)
		r.Header.Set("User-Agent", tc.ua)
		c, _ := ginTestContext(w, r)
		s.serveNodes(c, []sub.Node{node}, sub.Info{}, nil, converterConfig{External: true, Backend: backend.URL}, renderOpts{}, "/callback", nil)
		var doc map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &doc) != nil {
			t.Fatal("invalid subscription response", tc.ua, w.Code)
		}
		present := strings.Contains(w.Body.String(), `"userkey": "user-secret"`)
		if present != tc.want {
			t.Fatal("HTTP client filter wrong", tc.ua, present)
		}
	}
	if external.Load() != 0 {
		t.Fatal("Snell credentials sent to external link converter")
	}
}
