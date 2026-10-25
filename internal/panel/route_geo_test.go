package panel

import (
	"bytes"
	"compress/gzip"
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

func geoFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../geoip/testdata/GeoIP2-City-Test.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func uploadGeo(t *testing.T, c *apiClient, data []byte, query string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("PUT", c.base+"/tools/route-geo/database"+query, bytes.NewReader(data))
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

func installGeo(t *testing.T, s *Server, c *apiClient) string {
	t.Helper()
	data := geoFixture(t)
	if st, out := uploadGeo(t, c, data, ""); st != 200 || out["effectiveEnabled"] != false {
		t.Fatal(st, out)
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	if st, out := c.do("PUT", "/tools/route-geo", map[string]any{"enabled": true, "expectedRevision": 1}); st != 200 || out["effectiveEnabled"] != true {
		t.Fatal(st, out)
	}
	return hash
}

func TestRouteGeoConfiguration(t *testing.T) {
	s, c := routeScheduleFixture(t)
	if st, out := c.do("GET", "/tools/route-geo", nil); st != 200 || out["effectiveEnabled"] != false || out["databaseState"] != "not_configured" {
		t.Fatal(st, out)
	}
	if st, out := c.do("PUT", "/tools/route-geo", map[string]any{"enabled": true}); st != 409 {
		t.Fatal(st, out)
	}
	hash := installGeo(t, s, c)
	if st, out := c.do("PUT", "/tools/route-geo", map[string]any{"enabled": false, "expectedRevision": 1}); st != 409 {
		t.Fatal(st, out)
	}
	if st, out := uploadGeo(t, c, []byte("bad MMDB"), ""); st != 400 {
		t.Fatal(st, out)
	}
	if st, out := uploadGeo(t, c, geoFixture(t), "?expectedRevision=1"); st != 409 {
		t.Fatal(st, out)
	}
	if st, out := uploadGeo(t, c, geoFixture(t), "?expectedRevision=2"); st != 200 || out["policy"].(map[string]any)["revision"].(float64) != 2 {
		t.Fatal(st, out)
	}
	if st, out := c.do("GET", "/tools/route-geo", nil); st != 200 || out["policy"].(map[string]any)["sha256"] != hash {
		t.Fatal(st, out)
	}
	// Generic settings cannot point the service at an arbitrary local file.
	c.do("PUT", "/settings", map[string]string{routeGeoPolicyKey: `{"enabled":true,"sha256":"../../secret"}`})
	p, err := readRouteGeoPolicy(s.st.DB)
	if err != nil || p.SHA256 != hash {
		t.Fatal(p, err)
	}
	c.do("PUT", "/tools/route-geo", map[string]any{"enabled": false, "expectedRevision": 2})
	if st, out := c.do("GET", "/tools/route-geo", nil); st != 200 || out["effectiveEnabled"] != false || out["databaseState"] != "ready" {
		t.Fatal(st, out)
	}
}

func TestRouteGeoSnapshotAndAgentClaims(t *testing.T) {
	s, c := routeScheduleFixture(t)
	hash := installGeo(t, s, c)
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
				r := traceResult("81.2.69.160", "10.0.0.1", "81.2.69.160")
				r.Geo = &model.RouteTraceGeoMetadata{State: "ready", DatabaseSHA256: "forged"}
				r.Hops[1].Geo = []model.RouteTraceIPGeo{{State: "matched", CountryCode: "ZZ", CityEN: "forged"}}
				raw, _ := json.Marshal([]model.RouteTraceResult{r})
				s.hub.resolve("trace", &polarisv1.CommandResult{Id: cmd.Id, Ok: true, Output: string(raw)})
			}
		}
	}()
	request := map[string]any{"ipv4": "81.2.69.160"}
	st, out := c.do("POST", "/nodes/trace/route-trace", request)
	if st != 200 {
		t.Fatal(st, out)
	}
	id := int64(out["history"].([]any)[0].(map[string]any)["id"].(float64))
	var first store.NodeRouteTrace
	s.st.DB.First(&first, id)
	if first.Result.V.Geo.DatabaseSHA256 != hash || first.Result.V.Geo.IPVersion != 6 || first.Result.V.Hops[0].Geo[0].State != "private" || first.Result.V.Hops[1].Geo[0].CountryCode != "GB" {
		t.Fatal(first.Result.V)
	}
	originalCity := first.Result.V.Hops[1].Geo[0].CityEN
	replacement := bytes.ReplaceAll(geoFixture(t), []byte("London"), []byte("Oxford"))
	if st, out := uploadGeo(t, c, replacement, "?expectedRevision=2"); st != 200 {
		t.Fatal(st, out)
	}
	if st, out := c.do("POST", "/nodes/trace/route-trace", request); st != 200 || out["history"].([]any)[0].(map[string]any)["comparison"].(map[string]any)["state"] != "same" {
		t.Fatal("database change altered path comparison", st, out)
	}
	if st, out := c.do("GET", fmt.Sprintf("/nodes/trace/route-traces/%d", id), nil); st != 200 {
		t.Fatal(st, out)
	}
	s.st.DB.First(&first, id)
	if first.Result.V.Geo.DatabaseSHA256 != hash || first.Result.V.Hops[1].Geo[0].CityEN != originalCity {
		t.Fatal("history attribution recomputed")
	}
	c.do("PUT", "/tools/route-geo", map[string]any{"enabled": false})
	st, out = c.do("POST", "/nodes/trace/route-trace", request)
	if st != 200 {
		t.Fatal(st, out)
	}
	var latest store.NodeRouteTrace
	s.st.DB.Order("id DESC").First(&latest)
	if latest.Result.V.Geo.State != "disabled" || len(latest.Result.V.Hops[1].Geo) != 0 {
		t.Fatal("Agent attribution was trusted", latest.Result.V)
	}
}

func TestRouteGeoReplicaMissingAndHashMismatch(t *testing.T) {
	s, c := routeScheduleFixture(t)
	hash := installGeo(t, s, c)
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
	if st, out := second.do("GET", "/tools/route-geo", nil); st != 200 || out["effectiveEnabled"] != false || out["databaseState"] != "local_database_missing" {
		t.Fatal(st, out)
	}
	if st, out := second.do("POST", "/tools/route-geo/lookup", map[string]any{"ip": "81.2.69.160"}); st != 409 || out["databaseState"] != "local_database_missing" {
		t.Fatal(st, out)
	}
	results := []model.RouteTraceResult{traceResult("81.2.69.160", "81.2.69.160")}
	other.enrichRouteGeo(results)
	if results[0].State != "reached" || results[0].Geo.State != "unavailable" {
		t.Fatal(results)
	}
	dir := filepath.Join(cfg.DataDir, "route-geo")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, hash+".mmdb"), []byte("not the pinned database"), 0600)
	if st, out := second.do("GET", "/tools/route-geo", nil); st != 200 || out["databaseState"] != "hash_mismatch" {
		t.Fatal(st, out)
	}
	if st, out := second.do("POST", "/tools/route-geo/lookup", map[string]any{"ip": "81.2.69.160"}); st != 409 || out["databaseState"] != "hash_mismatch" {
		t.Fatal(st, out)
	}
	if st, out := uploadGeo(t, second, geoFixture(t), "?expectedRevision=2"); st != 200 || out["effectiveEnabled"] != true || out["policy"].(map[string]any)["revision"].(float64) != 2 {
		t.Fatal(st, out)
	}
}

func TestRouteGeoUploadRollbackAndLocalRetention(t *testing.T) {
	s, c := routeScheduleFixture(t)
	firstHash := installGeo(t, s, c)
	changed := bytes.ReplaceAll(geoFixture(t), []byte("London"), []byte("Oxford"))
	hook := "test:geo-policy-save-failure"
	s.st.DB.Callback().Update().Before("gorm:update").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "settings" {
			if values, ok := tx.Statement.Dest.(map[string]interface{}); ok && values["value"] != nil {
				tx.AddError(fmt.Errorf("injected save failure"))
			}
		}
	})
	st, _ := uploadGeo(t, c, changed, "")
	s.st.DB.Callback().Update().Remove(hook)
	if st != 500 {
		t.Fatal(st)
	}
	p, err := readRouteGeoPolicy(s.st.DB)
	if err != nil || p.SHA256 != firstHash {
		t.Fatal("failed import changed policy", p, err)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "route-geo", firstHash+".mmdb")); err != nil {
		t.Fatal("failed import removed active database", err)
	}
	if st, out := uploadGeo(t, c, changed, ""); st != 200 {
		t.Fatal(st, out)
	}
	third := bytes.ReplaceAll(geoFixture(t), []byte("London"), []byte("Berlin"))
	if st, out := uploadGeo(t, c, third, ""); st != 200 {
		t.Fatal(st, out)
	}
	files, err := filepath.Glob(filepath.Join(s.cfg.DataDir, "route-geo", "*.mmdb"))
	if err != nil || len(files) != 2 {
		t.Fatal(files, err)
	}
}

func TestRouteGeoBoundedEvidenceAndBadPolicy(t *testing.T) {
	s, c := routeScheduleFixture(t)
	installGeo(t, s, c)
	r := traceResult("81.2.69.160", "81.2.69.160")
	r.Hops[0].Addresses = []string{"81.2.69.160", "1.0.0.2", "1.0.0.3", "1.0.0.4", "1.0.0.5"}
	results := []model.RouteTraceResult{r}
	s.enrichRouteGeo(results)
	if !results[0].Geo.Limited || len(results[0].Hops[0].Geo) != 4 {
		t.Fatal(results)
	}
	s.st.SetSetting(routeGeoPolicyKey, "invalid JSON")
	s.enrichRouteGeo(results)
	if results[0].Geo.State != "unavailable" || len(results[0].Hops[0].Geo) != 0 || results[0].State != "reached" {
		t.Fatal(results)
	}
	if st, out := c.do("PUT", "/tools/route-geo", map[string]any{"enabled": false}); st != 200 || out["effectiveEnabled"] != false {
		t.Fatal(st, out)
	}
}

func TestRouteGeoToolAuthorization(t *testing.T) {
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
			if st, out := client.do(method, "/tools/route-geo", nil); st != want {
				t.Fatal(st, out)
			}
		}
		if st, out := client.do("POST", "/tools/route-geo/lookup", map[string]any{"ip": "81.2.69.160"}); st != want {
			t.Fatal(st, out)
		}
		if st, out := uploadGeo(t, client, geoFixture(t), ""); st != want {
			t.Fatal(st, out)
		}
	}
}

func TestRouteGeoRejectsOversizedUpload(t *testing.T) {
	s, c := routeScheduleFixture(t)
	request := httptest.NewRequest("PUT", c.base+"/tools/route-geo/database", bytes.NewReader([]byte("x")))
	request.ContentLength = (128 << 20) + 1
	request.Header.Set("Authorization", "Bearer "+c.tok)
	recorder := httptest.NewRecorder()
	s.Router().ServeHTTP(recorder, request)
	if recorder.Code != 413 {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func TestRouteGeoLookupPreview(t *testing.T) {
	s, c := routeScheduleFixture(t)
	if st, out := c.do("POST", "/tools/route-geo/lookup", map[string]any{"ip": "81.2.69.160"}); st != 409 || out["databaseState"] != "not_configured" {
		t.Fatal(st, out)
	}
	if st, out := uploadGeo(t, c, geoFixture(t), "?expectedRevision=0"); st != 200 {
		t.Fatal(st, out)
	}
	prior, err := readRouteGeoPolicy(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ip, state string
		number    string
	}{
		{" 81.2.69.160 ", "matched", "GB"},
		{"2001:218::1", "matched", "JP"},
		{"::ffff:81.2.69.160", "matched", "GB"},
		{"10.0.0.1", "private", ""},
		{"::1", "private", ""},
		{"203.0.113.1", "not_found", ""},
	} {
		st, out := c.do("POST", "/tools/route-geo/lookup", map[string]any{"ip": tc.ip})
		if st != 200 || out["effectiveEnabled"] != false || out["databaseSHA256"] != prior.SHA256 {
			t.Fatal(st, out)
		}
		match := out["match"].(map[string]any)
		if match["state"] != tc.state || (tc.number != "" && (match["countryCode"] != tc.number || match["country"] == "" || match["network"] == "")) {
			t.Fatal(tc, match)
		}
	}
	for _, ip := range []string{"", "example.com", "https://example.com/", "1.0.0.1:443", "1.0.0.1/24", "fe80::1%eth0"} {
		if st, out := c.do("POST", "/tools/route-geo/lookup", map[string]any{"ip": ip}); st != 400 {
			t.Fatal(st, out)
		}
	}
	after, err := readRouteGeoPolicy(s.st.DB)
	if err != nil || after != prior {
		t.Fatal("preview modified policy", after, err)
	}
	if st, out := c.do("PUT", "/tools/route-geo", map[string]any{"enabled": true, "expectedRevision": prior.Revision}); st != 200 {
		t.Fatal(st, out)
	}
	if st, out := c.do("POST", "/tools/route-geo/lookup", map[string]any{"ip": "81.2.69.160"}); st != 200 || out["effectiveEnabled"] != true {
		t.Fatal(st, out)
	}
}

func TestRouteGeoGzipAndPolicyRecovery(t *testing.T) {
	s, c := routeScheduleFixture(t)
	raw := geoFixture(t)
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	hash := hex.EncodeToString(digest[:])
	if st, out := uploadGeo(t, c, compressed.Bytes(), "?expectedRevision=0"); st != 200 || out["policy"].(map[string]any)["sha256"] != hash {
		t.Fatal(st, out)
	}
	if st, out := uploadGeo(t, c, raw, "?expectedRevision=1"); st != 200 || out["policy"].(map[string]any)["revision"] != float64(1) {
		t.Fatal(st, out)
	}
	// Corrupt compressed input cannot replace the valid database or its policy.
	bad := append([]byte(nil), compressed.Bytes()...)
	bad[len(bad)-1] ^= 0xff
	if st, out := uploadGeo(t, c, bad, "?expectedRevision=1"); st != 400 {
		t.Fatal(st, out)
	}
	if p, err := readRouteGeoPolicy(s.st.DB); err != nil || p.SHA256 != hash || p.Enabled {
		t.Fatal(p, err)
	}
	if p, err := readRouteASNPolicy(s.st.DB); err != nil || p.SHA256 != "" || p.Enabled {
		t.Fatal("city upload changed ASN settings", p, err)
	}
	s.st.SetSetting(routeGeoPolicyKey, "broken JSON")
	if st, out := c.do("GET", "/tools/route-geo", nil); st != 200 || out["databaseState"] != "invalid_policy" || out["effectiveEnabled"] != false {
		t.Fatal(st, out)
	}
	if st, out := uploadGeo(t, c, raw, "?expectedRevision=0"); st != 200 || out["effectiveEnabled"] != false {
		t.Fatal(st, out)
	}
}
