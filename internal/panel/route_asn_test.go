package panel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func asnFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../asn/testdata/GeoLite2-ASN-Test.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func uploadASN(t *testing.T, c *apiClient, data []byte, query string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("PUT", c.base+"/tools/route-asn/database"+query, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+c.tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, out
}

func installASN(t *testing.T, s *Server, c *apiClient) string {
	t.Helper()
	data := asnFixture(t)
	if st, out := uploadASN(t, c, data, ""); st != 200 || out["effectiveEnabled"] != false {
		t.Fatal(st, out)
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	if st, out := c.do("PUT", "/tools/route-asn", map[string]any{"enabled": true, "expectedRevision": 1}); st != 200 || out["effectiveEnabled"] != true {
		t.Fatal(st, out)
	}
	return hash
}

func TestRouteASNConfiguration(t *testing.T) {
	s, c := routeScheduleFixture(t)
	if st, out := c.do("GET", "/tools/route-asn", nil); st != 200 || out["effectiveEnabled"] != false || out["databaseState"] != "not_configured" {
		t.Fatal(st, out)
	}
	if st, out := c.do("PUT", "/tools/route-asn", map[string]any{"enabled": true}); st != 409 {
		t.Fatal(st, out)
	}
	hash := installASN(t, s, c)
	if st, out := c.do("PUT", "/tools/route-asn", map[string]any{"enabled": false, "expectedRevision": 1}); st != 409 {
		t.Fatal(st, out)
	}
	if st, out := uploadASN(t, c, []byte("bad MMDB"), ""); st != 400 {
		t.Fatal(st, out)
	}
	if st, out := uploadASN(t, c, asnFixture(t), "?expectedRevision=1"); st != 409 {
		t.Fatal(st, out)
	}
	if st, out := uploadASN(t, c, asnFixture(t), "?expectedRevision=2"); st != 200 || out["policy"].(map[string]any)["revision"].(float64) != 2 {
		t.Fatal(st, out)
	}
	if st, out := c.do("GET", "/tools/route-asn", nil); st != 200 || out["policy"].(map[string]any)["sha256"] != hash {
		t.Fatal(st, out)
	}
	// Generic settings cannot point the service at an arbitrary local file.
	c.do("PUT", "/settings", map[string]string{routeASNPolicyKey: `{"enabled":true,"sha256":"../../secret"}`})
	p, err := readRouteASNPolicy(s.st.DB)
	if err != nil || p.SHA256 != hash {
		t.Fatal(p, err)
	}
	c.do("PUT", "/tools/route-asn", map[string]any{"enabled": false, "expectedRevision": 2})
	if st, out := c.do("GET", "/tools/route-asn", nil); st != 200 || out["effectiveEnabled"] != false || out["databaseState"] != "ready" {
		t.Fatal(st, out)
	}
}

func TestRouteASNSnapshotAndAgentClaims(t *testing.T) {
	s, c := routeScheduleFixture(t)
	hash := installASN(t, s, c)
	conn := s.hub.register("trace")
	defer s.hub.unregister(conn)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case msg := <-conn.out:
				cmd := msg.GetCommand()
				if cmd == nil {
					continue
				}
				r := traceResult("1.0.0.1", "10.0.0.1", "1.0.0.1")
				r.ASN = &model.RouteTraceASNMetadata{State: "ready", DatabaseSHA256: "forged"}
				r.Hops[1].ASN = []model.RouteTraceIPASN{{State: "matched", ASN: 99999, Organization: "forged"}}
				raw, _ := json.Marshal([]model.RouteTraceResult{r})
				s.hub.resolve("trace", &polarisv1.CommandResult{Id: cmd.Id, Ok: true, Output: string(raw)})
			}
		}
	}()
	request := map[string]any{"ipv4": "1.0.0.1"}
	st, out := c.do("POST", "/nodes/trace/route-trace", request)
	if st != 200 {
		t.Fatal(st, out)
	}
	id := int64(out["history"].([]any)[0].(map[string]any)["id"].(float64))
	var first store.NodeRouteTrace
	s.st.DB.First(&first, id)
	if first.Result.V.ASN.DatabaseSHA256 != hash || first.Result.V.ASN.IPVersion != 6 || first.Result.V.Hops[0].ASN[0].State != "private" || first.Result.V.Hops[1].ASN[0].ASN != 15169 {
		t.Fatal(first.Result.V)
	}
	originalOrg := first.Result.V.Hops[1].ASN[0].Organization
	replacement := bytes.ReplaceAll(asnFixture(t), []byte("Google Inc."), []byte("Google LLC."))
	if st, out := uploadASN(t, c, replacement, "?expectedRevision=2"); st != 200 {
		t.Fatal(st, out)
	}
	if st, out := c.do("POST", "/nodes/trace/route-trace", request); st != 200 || out["history"].([]any)[0].(map[string]any)["comparison"].(map[string]any)["state"] != "same" {
		t.Fatal("database change altered path comparison", st, out)
	}
	if st, out := c.do("GET", fmt.Sprintf("/nodes/trace/route-traces/%d", id), nil); st != 200 {
		t.Fatal(st, out)
	}
	s.st.DB.First(&first, id)
	if first.Result.V.ASN.DatabaseSHA256 != hash || first.Result.V.Hops[1].ASN[0].Organization != originalOrg {
		t.Fatal("history attribution recomputed")
	}
	c.do("PUT", "/tools/route-asn", map[string]any{"enabled": false})
	st, out = c.do("POST", "/nodes/trace/route-trace", request)
	if st != 200 {
		t.Fatal(st, out)
	}
	var latest store.NodeRouteTrace
	s.st.DB.Order("id DESC").First(&latest)
	if latest.Result.V.ASN.State != "disabled" || len(latest.Result.V.Hops[1].ASN) != 0 {
		t.Fatal("Agent attribution was trusted", latest.Result.V)
	}
}

func TestRouteASNReplicaMissingAndHashMismatch(t *testing.T) {
	s, c := routeScheduleFixture(t)
	hash := installASN(t, s, c)
	cfg := *s.cfg
	cfg.DataDir = t.TempDir()
	other, err := New(&cfg, s.log, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db, _ := other.st.DB.DB(); db.Close() }()
	server := httptest.NewServer(other.Router())
	defer server.Close()
	second := &apiClient{t: t, base: server.URL + cfg.APIBase(), tok: c.tok}
	if st, out := second.do("GET", "/tools/route-asn", nil); st != 200 || out["effectiveEnabled"] != false || out["databaseState"] != "local_database_missing" {
		t.Fatal(st, out)
	}
	results := []model.RouteTraceResult{traceResult("1.0.0.1", "1.0.0.1")}
	other.enrichRouteASN(results)
	if results[0].State != "reached" || results[0].ASN.State != "unavailable" {
		t.Fatal(results)
	}
	dir := filepath.Join(cfg.DataDir, "route-asn")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, hash+".mmdb"), []byte("not the pinned database"), 0600)
	if st, out := second.do("GET", "/tools/route-asn", nil); st != 200 || out["databaseState"] != "hash_mismatch" {
		t.Fatal(st, out)
	}
	if st, out := uploadASN(t, second, asnFixture(t), "?expectedRevision=2"); st != 200 || out["effectiveEnabled"] != true || out["policy"].(map[string]any)["revision"].(float64) != 2 {
		t.Fatal(st, out)
	}
}

func TestRouteASNUploadRollbackAndLocalRetention(t *testing.T) {
	s, c := routeScheduleFixture(t)
	firstHash := installASN(t, s, c)
	changed := bytes.ReplaceAll(asnFixture(t), []byte("Google Inc."), []byte("Google LLC."))
	hook := "test:asn-policy-save-failure"
	s.st.DB.Callback().Update().Before("gorm:update").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "settings" {
			if values, ok := tx.Statement.Dest.(map[string]interface{}); ok && values["value"] != nil {
				tx.AddError(fmt.Errorf("injected save failure"))
			}
		}
	})
	st, _ := uploadASN(t, c, changed, "")
	s.st.DB.Callback().Update().Remove(hook)
	if st != 500 {
		t.Fatal(st)
	}
	p, err := readRouteASNPolicy(s.st.DB)
	if err != nil || p.SHA256 != firstHash {
		t.Fatal("failed import changed policy", p, err)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "route-asn", firstHash+".mmdb")); err != nil {
		t.Fatal("failed import removed active database", err)
	}
	if st, out := uploadASN(t, c, changed, ""); st != 200 {
		t.Fatal(st, out)
	}
	third := bytes.ReplaceAll(asnFixture(t), []byte("Google Inc."), []byte("Google XXX."))
	if st, out := uploadASN(t, c, third, ""); st != 200 {
		t.Fatal(st, out)
	}
	files, err := filepath.Glob(filepath.Join(s.cfg.DataDir, "route-asn", "*.mmdb"))
	if err != nil || len(files) != 2 {
		t.Fatal(files, err)
	}
}

func TestRouteASNBoundedEvidenceAndBadPolicy(t *testing.T) {
	s, c := routeScheduleFixture(t)
	installASN(t, s, c)
	r := traceResult("1.0.0.1", "1.0.0.1")
	r.Hops[0].Addresses = []string{"1.0.0.1", "1.0.0.2", "1.0.0.3", "1.0.0.4", "1.0.0.5"}
	results := []model.RouteTraceResult{r}
	s.enrichRouteASN(results)
	if !results[0].ASN.Limited || len(results[0].Hops[0].ASN) != 4 {
		t.Fatal(results)
	}
	s.st.SetSetting(routeASNPolicyKey, "invalid JSON")
	s.enrichRouteASN(results)
	if results[0].ASN.State != "unavailable" || len(results[0].Hops[0].ASN) != 0 || results[0].State != "reached" {
		t.Fatal(results)
	}
	if st, out := c.do("PUT", "/tools/route-asn", map[string]any{"enabled": false}); st != 200 || out["effectiveEnabled"] != false {
		t.Fatal(st, out)
	}
}

func TestRouteASNToolAuthorization(t *testing.T) {
	_, c := routeScheduleFixture(t)
	st, out := c.do("POST", "/tokens", map[string]any{"name": "nodes-reader", "scopes": []string{ScopeNodesRead}})
	if st != 201 {
		t.Fatal(st, out)
	}
	token := &apiClient{t: t, base: c.base, tok: out["secret"].(string)}
	anon := &apiClient{t: t, base: c.base}
	for _, client := range []*apiClient{token, anon} {
		want := 403
		if client == anon {
			want = 401
		}
		for _, method := range []string{"GET", "PUT"} {
			if st, out := client.do(method, "/tools/route-asn", nil); st != want {
				t.Fatal(st, out)
			}
		}
		if st, out := uploadASN(t, client, asnFixture(t), ""); st != want {
			t.Fatal(st, out)
		}
	}
}

func TestRouteASNRejectsOversizedUpload(t *testing.T) {
	s, c := routeScheduleFixture(t)
	request := httptest.NewRequest("PUT", c.base+"/tools/route-asn/database", bytes.NewReader([]byte("x")))
	request.ContentLength = (64 << 20) + 1
	request.Header.Set("Authorization", "Bearer "+c.tok)
	recorder := httptest.NewRecorder()
	s.Router().ServeHTTP(recorder, request)
	if recorder.Code != 413 {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}
