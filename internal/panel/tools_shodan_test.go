package panel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/realityscan"
	"github.com/gin-gonic/gin"
)

func TestRealityDiscover(t *testing.T) {
	// A local TLS 1.3 + h2 server stands in for a public site.
	site := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	site.EnableHTTP2 = true
	site.StartTLS()
	defer site.Close()
	port, _ := strconv.Atoi(site.URL[strings.LastIndex(site.URL, ":")+1:])

	var calls int
	shodan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("key") != "sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api-info" {
			w.Write([]byte(`{"query_credits":42,"plan":"membership"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"total": 1, "matches": []map[string]any{{"ip_str": "127.0.0.1", "port": port, "ssl": map[string]any{"cert": map[string]any{"subject": map[string]string{"CN": "example.invalid"}}}}}})
	}))
	defer shodan.Close()
	old := realityscan.ShodanBase
	realityscan.ShodanBase = shodan.URL
	defer func() { realityscan.ShodanBase = old }()

	s, _, admin := newAdminClient(t)

	// No key yet: a clear error, and Shodan is not called.
	if st, out := admin.do("GET", "/tools/shodan", nil); st != 200 || out["configured"] != false {
		t.Fatalf("info without key: %d %v", st, out)
	}
	if st, out := admin.do("POST", "/tools/reality-discover", map[string]any{}); st != 400 || !strings.Contains(out["error"].(string), "Shodan API Key") {
		t.Fatalf("discover without key: %d %v", st, out)
	}
	if calls != 0 {
		t.Fatal("Shodan called without a key")
	}

	// The key is stored sealed and never returned.
	s.st.SetSetting("shodan_api_key", s.st.Box.Seal("sk-test"))
	if raw := s.st.GetSetting("shodan_api_key", ""); raw == "sk-test" {
		t.Fatal("key stored in clear")
	}
	st, out := admin.do("GET", "/settings", nil)
	if st != 200 || out["shodan_api_key_set"] != true || strings.Contains(toJSONString(out), "sk-test") {
		t.Fatalf("settings: %d %v", st, out)
	}
	if st, out := admin.do("GET", "/tools/shodan", nil); st != 200 || out["credits"].(float64) != 42 {
		t.Fatalf("info: %d %v", st, out)
	}

	// Discovery hands Shodan's hosts to the normal TLS check.
	st, out = admin.do("POST", "/tools/reality-discover", map[string]any{"country": "JP"})
	if st != 200 {
		t.Fatalf("discover: %d %v", st, out)
	}
	if out["query"] != realityscan.DefaultShodanQuery+" country:JP" {
		t.Fatalf("query %v", out["query"])
	}
	cands := out["candidates"].([]any)
	if len(cands) != 1 || cands[0] != "127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("candidates %v", cands)
	}
	rs := out["results"].([]any)
	if len(rs) != 1 {
		t.Fatalf("results %v", rs)
	}
	r := rs[0].(map[string]any)
	if r["target"] != "127.0.0.1:"+strconv.Itoa(port) || r["serverName"] != "example.invalid" || r["ok"] != false || r["tls"] != "1.3" || r["alpn"] != "h2" || out["from"] != "panel" {
		t.Fatalf("checked result %v from %v", r, out["from"])
	}
	if strings.Contains(toJSONString(out), "sk-test") {
		t.Fatal("key in the response")
	}
}

func TestRealityScanNodeSourceAndCompatibility(t *testing.T) {
	var handshakes atomic.Int32
	site := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	site.EnableHTTP2 = true
	site.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		handshakes.Add(1)
		return nil, nil
	}}
	site.StartTLS()
	defer site.Close()
	address := strings.TrimPrefix(site.URL, "https://")
	candidate := realityscan.Candidate{Address: address, ServerName: "example.invalid", ASN: "AS64500"}
	for _, mode := range []string{"new", "old", "failed", "malformed", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestServer(t)
			conn := s.hub.register("node")
			defer s.hub.unregister(conn)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			observed := make(chan *polarisv1.Command, 1)
			go func() {
				select {
				case m := <-conn.out:
					cmd := m.GetCommand()
					observed <- cmd
					if mode == "canceled" {
						cancel()
						return
					}
					r := &polarisv1.CommandResult{Id: cmd.Id, Ok: true, Output: `[]`}
					switch mode {
					case "old":
						r.Ok, r.Error = false, "unknown command: reality_scan_v2"
					case "failed":
						r.Ok, r.Error = false, "node scan failed"
					case "malformed":
						r.Output = `null`
					}
					s.hub.resolve("node", r)
				case <-t.Context().Done():
				}
			}()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(ctx, "POST", "/tools/reality-discover", nil)
			before := handshakes.Load()
			_, from, err := s.scanReality(c, "", []realityscan.Candidate{candidate}, "node")
			cmd := <-observed
			var received []realityscan.Candidate
			if cmd.Kind != "reality_scan_v2" || json.Unmarshal([]byte(cmd.Args["candidates"]), &received) != nil || len(received) != 1 || received[0] != candidate {
				t.Fatalf("node did not receive endpoint/SNI pair: %+v", cmd)
			}
			switch mode {
			case "new":
				if err != nil || from != "node" || handshakes.Load() != before {
					t.Fatalf("new Agent incorrectly fell back: %s %v", from, err)
				}
			case "old":
				if err != nil || from != "panel-fallback-unsupported" || handshakes.Load() <= before {
					t.Fatalf("old Agent fallback not identified: %s %v", from, err)
				}
			default:
				if err == nil || handshakes.Load() != before {
					t.Fatalf("failed node scan silently repeated from panel: %s %v", from, err)
				}
			}
		})
	}
}
