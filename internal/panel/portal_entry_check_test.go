package panel

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func TestPortalEntryCheckActualSocketAndTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)
	n := sub.Node{Server: host, Port: p, Protocol: model.ProtoVLESS, TLS: model.ClientTLS{Type: model.SecTLS, Insecure: true}}
	dial := (&net.Dialer{}).DialContext
	checked := checkPortalEntry(t.Context(), n, dial)
	if checked.State != "passed" || checked.Stage != "tls" || !strings.Contains(checked.Message, "尚未验证代理认证") {
		t.Fatal(checked)
	}
	n.TLS.Insecure = false
	if result := checkPortalEntry(t.Context(), n, dial); result.State != "failed" || result.Stage != "tls" {
		t.Fatal("invalid certificate was accepted", result)
	}
	n.TLS.Type = model.SecReality
	if result := checkPortalEntry(t.Context(), n, dial); result.State != "passed" || result.Stage != "tcp" || !strings.Contains(result.Message, "尚未验证 REALITY") {
		t.Fatal("TCP became REALITY proof", result)
	}
	n.Protocol = model.ProtoTUIC
	called := false
	result := checkPortalEntry(t.Context(), n, func(context.Context, string, string) (net.Conn, error) { called = true; return nil, nil })
	if result.State != "unsupported" || called {
		t.Fatal("UDP was tested using TCP", result)
	}
	n.Protocol = model.ProtoVLESS
	if result := checkPortalEntry(t.Context(), n, portalEntryDial); result.State != "unsupported" || strings.Contains(result.Message, host) {
		t.Fatal("private socket guard failed", result)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result := checkPortalEntry(ctx, n, dial); result.State != "failed" {
		t.Fatal("cancelled check passed", result)
	}
}

func TestPortalEntryCheckAuthorizationCacheAndLimits(t *testing.T) {
	s, base, _ := cdnRegionFixture(t)
	client := regionPortalClient(t, s, base, "ra")
	// A caller supplies an opaque published key, never an address or port.
	code, _ := client.req("POST", "/u/nodes/check", map[string]string{"key": "foreign-node"}, true)
	if code != 404 {
		t.Fatal("unpublished target allowed", code)
	}
	code, _ = client.req("POST", "/u/nodes/check", map[string]string{"key": "foreign-node"}, false)
	if code != 403 {
		t.Fatal("missing CSRF check", code)
	}
	code, body := client.req("GET", "/u/nodes/status", nil, false)
	var status struct {
		Items []struct {
			Key string `json:"key"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &status); err != nil || code != 200 || len(status.Items) == 0 {
		t.Fatal(code, body, err)
	}
	// A cached socket result remains user-scoped and never becomes business proof.
	key := status.Items[0].Key
	result := portalEntryCheck{State: "passed", Stage: "tcp", Message: "fixture connection proof", CheckedAt: time.Now().UTC()}
	if err := s.eph.put(portalCheckCacheKey("ra", key), result, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	_, body = client.req("GET", "/u/nodes/status", nil, false)
	if !strings.Contains(body, "fixture connection proof") || !strings.Contains(body, `"business":"unknown"`) {
		t.Fatal("socket proof mixed with business", body)
	}
	other := regionPortalClient(t, s, base, "rb")
	_, body = other.req("GET", "/u/nodes/status", nil, false)
	if strings.Contains(body, "fixture connection proof") {
		t.Fatal("check evidence leaked to another user")
	}
	for range 10 {
		s.shared.allow("portal-entry-user", 6, "ra")
	}
	if code, _ := client.req("POST", "/u/nodes/check", map[string]string{"key": key}, true); code != 429 {
		t.Fatal("missing request limit", code)
	}
}
