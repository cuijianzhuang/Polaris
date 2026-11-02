package panel

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func personalState(t *testing.T, c *portalClient) map[string]any {
	t.Helper()
	code, body := c.req("GET", "/u/cdn-state?profileId=regional", nil, false)
	if code != 200 {
		t.Fatal(code, body)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(body), &state); err != nil {
		t.Fatal(err)
	}
	return state
}
func TestPersonalCDNOptimisticEdits(t *testing.T) {
	s, base, _ := cdnRegionFixture(t)
	a := regionPortalClient(t, s, base, "ra")
	b := regionPortalClient(t, s, base, "ra")
	version := personalState(t, a)["version"]
	save := map[string]any{"profileId": "regional", "version": version, "addresses": []string{"104.20.9.9"}, "disabled": []string{}, "stalePolicy": "fallback", "staleHours": 12}
	if code, out := a.req("POST", "/u/cdn-preferences", save, true); code != 200 {
		t.Fatal(code, out)
	}
	for _, edit := range []map[string]any{
		{"profileId": "regional", "version": version, "addresses": []string{"104.20.8.8"}},
		{"profileId": "regional", "version": version, "addresses": []string{}, "disabled": []string{"104.20.9.9"}},
		{"profileId": "regional", "version": version, "addresses": []string{}, "stalePolicy": "keep"},
	} {
		if code, out := b.req("POST", "/u/cdn-preferences", edit, true); code != 409 {
			t.Fatal("stale edit accepted", code, out)
		}
	}
	if code, _ := a.req("POST", "/u/cdn-preferences", map[string]any{"profileId": "regional", "version": "", "addresses": []string{}}, true); code != 428 {
		t.Fatal("unversioned edit accepted", code)
	}
	snapshot := personalState(t, b)
	before := snapshot["version"]
	// Live probe/check writes are independent of the version guarding manual edits.
	if err := s.mutatePersonalCDN("ra", "regional", func(_db *gorm.DB, pool *store.PersonalCDNPool) error {
		pool.Automatic = map[string][]string{"443": {"104.20.7.7"}}
		pool.EndpointChecks = map[string]store.PersonalCDNCheck{"test": {At: time.Now()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if after := personalState(t, b)["version"]; after != before {
		t.Fatal("measurement caused false conflict")
	}
	if code, out := b.req("POST", "/u/cdn-preferences", map[string]any{"profileId": "regional", "version": before, "addresses": []string{"104.20.6.6"}}, true); code != 200 {
		t.Fatal(code, out)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "ra")
	if !slices.Equal(u.CDNPool.V["regional"].Automatic["443"], []string{"104.20.7.7"}) || u.CDNPool.V["regional"].StalePolicy != "fallback" {
		t.Fatal("manual update overwrote reports or policy")
	}
	for _, bad := range []map[string]any{{"stalePolicy": "delete"}, {"staleHours": 0}, {"staleHours": 721}} {
		bad["profileId"] = "regional"
		bad["version"] = personalState(t, a)["version"]
		bad["addresses"] = []string{}
		if code, _ := a.req("POST", "/u/cdn-preferences", bad, true); code != 400 {
			t.Fatal("invalid policy accepted", code)
		}
	}
}

func TestPersonalCDNExpiryByPort(t *testing.T) {
	s, base, admin := cdnRegionFixture(t)
	a := regionPortalClient(t, s, base, "ra")
	old := time.Now().Add(-48 * time.Hour)
	pool := store.PersonalCDNPool{StalePolicy: "fallback", StaleHours: 24, Automatic: map[string][]string{"443": {"104.20.9.9"}, "2053": {"104.20.8.8"}}, ReportedAt: &old}
	s.st.DB.Model(&store.User{}).Where("id = ?", "ra").Update("cdn_pool", store.NewJSON(map[string]store.PersonalCDNPool{"regional": pool}))
	version := personalState(t, a)["version"]
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"104.16.1.1"}) {
		t.Fatal("expired report used", got)
	}
	_, secret, err := s.issueAPITokenIn(s.st.DB, "expiry test", "", []string{ScopeCDNIPs}, 0, "test", func(token *store.APIToken) {
		token.CDNUserID = "ra"
		token.CDNProfileID = "regional"
		token.CDNSource = "personal"
		token.CDNPort = 443
	})
	if err != nil {
		t.Fatal(err)
	}
	probe := &apiClient{t: t, base: admin.base, tok: secret}
	if code, out := probe.do("POST", "/cdn-profiles/regional/preferred-ips", map[string]any{"source": "personal", "port": 443, "scheme": "https", "ips": []map[string]any{{"ip": "104.20.7.7"}}}); code != 200 {
		t.Fatal(code, out)
	}
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"104.20.7.7"}) {
		t.Fatal("fresh report ignored", got)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "ra")
	current := u.CDNPool.V["regional"]
	if !personalCDNExpired(current, 2053, time.Now()) || personalCDNExpired(current, 443, time.Now()) {
		t.Fatal("freshness crossed port boundary", current.PortReports)
	}
	if got := personalCDNAutomatic(current, 443, "http", time.Now()); len(got) != 0 {
		t.Fatal("protocol mismatch used", got)
	}
	if after := personalState(t, a)["version"]; after != version {
		t.Fatal("probe invalidated edit version")
	}
	if code, out := a.req("POST", "/u/cdn-preferences", map[string]any{"profileId": "regional", "version": version, "addresses": []string{"keep.example.com"}}, true); code != 200 {
		t.Fatal(code, out)
	}
	// Even after all automatic measurements expire, the manually maintained address remains.
	s.st.DB.Model(&store.User{}).Where("id = ?", "ra").Update("cdn_pool", store.NewJSON(map[string]store.PersonalCDNPool{"regional": pool}))
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"keep.example.com"}) {
		t.Fatal("manual address expired", got)
	}
	pool.StalePolicy = "keep"
	if got := personalCDNAutomatic(pool, 2053, "https", time.Now()); !slices.Equal(got, []string{"104.20.8.8"}) {
		t.Fatal("keep policy ignored", got)
	}
	boundary := store.PersonalCDNPool{StaleHours: 1, PortReports: map[string]store.PersonalCDNReportMeta{"443": {At: old}}}
	if !personalCDNExpired(boundary, 443, old.Add(time.Hour)) {
		t.Fatal("expiry boundary not enforced")
	}
}

func TestPersonalCDNEntryScopedResultsAndPreview(t *testing.T) {
	s, base, _ := cdnRegionFixture(t)
	a := regionPortalClient(t, s, base, "ra")
	s.st.DB.Create(&store.Endpoint{ID: "other-entry", InboundID: "ri", Kind: "cdn", CDNProfileID: "regional", Host: "other.example.com", SNI: "other-sni.example.com", Path: "/other"})
	s.st.DB.Create(&store.Inbound{ID: "hidden-inbound", NodeID: "rn", Enabled: true, Port: 443, Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecTLS})})
	s.st.DB.Create(&store.Endpoint{ID: "hidden-entry", InboundID: "hidden-inbound", Kind: "cdn", CDNProfileID: "regional"})
	if code, out := a.req("POST", "/u/cdn-preferences", map[string]any{"profileId": "regional", "addresses": []string{"104.20.9.9"}}, true); code != 200 {
		t.Fatal(code, out)
	}
	var u store.User
	var p store.CDNProfile
	s.st.DB.First(&u, "id = ?", "ra")
	s.st.DB.First(&p, "id = ?", "regional")
	entries, err := s.personalCDNEntries(s.st.DB, &u, &p)
	if err != nil || len(entries) != 2 {
		t.Fatal("entry scope", entries, err)
	}
	var entry personalCDNEntry
	for _, e := range entries {
		if e.EndpointID == "re" {
			entry = e
		}
	}
	target := entry.Target
	target.Server = "104.20.9.9"
	pool := store.PersonalCDNPool{EndpointChecks: map[string]store.PersonalCDNCheck{cdnFailureSlot("re", target.Server): {At: time.Now(), Failed: true, Fingerprint: cdnCheckKey("re", target, entry.Transport, entry.Revision)}}}
	s.st.DB.Model(&u).Update("cdn_pool", store.NewJSON(map[string]store.PersonalCDNPool{"regional": pool}))
	code, body := a.req("POST", "/u/cdn-pool", map[string]any{"profileId": "regional", "action": "preview"}, true)
	if code != 200 {
		t.Fatal(code, body)
	}
	var preview struct {
		Entries []personalCDNPreviewEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Entries) != 2 || strings.Contains(body, "hidden-entry") || strings.Contains(body, u.UUID) || strings.Contains(body, u.SubToken) {
		t.Fatal("preview leaked grants or credentials", body)
	}
	for _, e := range preview.Entries {
		if e.EndpointID == "re" {
			if !slices.Equal(e.Selected, []string{"104.16.1.1"}) || e.Mode != "public" {
				t.Fatal("preview disagrees with fallback", e)
			}
			index := slices.IndexFunc(e.Rows, func(r personalCDNPreviewRow) bool { return r.Address == target.Server })
			if index < 0 || e.Rows[index].Reason != "check_failed" {
				t.Fatal("missing exclusion reason", e)
			}
		} else if !slices.Equal(e.Selected, []string{target.Server}) || e.Target.SNI != "other-sni.example.com" || e.Target.Host != "other.example.com" || e.Target.Path != "/other" {
			t.Fatal("failure or identity crossed entries", e)
		}
	}
	for _, change := range []map[string]any{{"sni": "changed.example.com"}, {"host": "changed-host.example.com"}, {"path": "/changed"}, {"port": 2053}} {
		s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "re").Updates(change)
		got := regionServers(t, s, "ra", "ct", "CN-31")
		if !slices.Equal(got, []string{target.Server, target.Server}) {
			t.Fatal("stale failure survived target change", change, got)
		}
		s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "re").Updates(map[string]any{"sni": "", "host": "", "path": "", "port": 0})
	}
	for _, badEntry := range []string{"hidden-entry", "unknown"} {
		if code, _ := a.req("POST", "/u/cdn-pool", map[string]any{"profileId": "regional", "action": "check", "endpointId": badEntry, "address": target.Server}, true); code != 403 {
			t.Fatal("unauthorized entry accepted", code)
		}
	}
	if code, _ := a.req("POST", "/u/cdn-pool", map[string]any{"profileId": "regional", "action": "check", "endpointId": "re", "context": "stale", "address": target.Server}, true); code != 409 {
		t.Fatal("old context accepted", code)
	}
}

func TestPersonalCDNExactTransportProbe(t *testing.T) {
	var seenSNI, seenHost, seenPath string
	edge := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenSNI, seenHost, seenPath = r.TLS.ServerName, r.Host, r.URL.Path
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Sec-WebSocket-Accept", cdnWebSocketAccept(r.Header.Get("Sec-WebSocket-Key")))
		w.WriteHeader(101)
	}))
	defer edge.Close()
	roots := x509.NewCertPool()
	roots.AddCert(edge.Certificate())
	previous := cdnProbeRoots
	cdnProbeRoots = roots
	defer func() { cdnProbeRoots = previous }()
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(edge.URL, "https://"))
	port, _ := strconv.Atoi(portText)
	target := cdnTarget{Provider: "other", Scheme: "https", Server: host, Port: port, SNI: "example.com", Host: "host.example.com", Path: "/actual-path"}
	// httptest's default certificate covers example.com; use an explicit DNS name
	// and verify the exact Host/path and validated SNI sent over the connection.
	result := probeCDNWithNetwork(context.Background(), target, model.TransWS, "", &net.Dialer{Timeout: time.Second}, func(_ context.Context, host string) ([]string, error) {
		t.Errorf("IP connection unnecessarily resolved SNI: %s", host)
		return []string{"127.0.0.1"}, nil
	}, false)
	if !result.OK || seenSNI != "example.com" || seenHost != "host.example.com" || seenPath != "/actual-path" {
		t.Fatal("target mismatch", result, seenSNI, seenHost, seenPath)
	}
}

func TestPersonalCDNPartialAndOutOfOrderChecks(t *testing.T) {
	now := time.Now()
	pool := store.PersonalCDNPool{}
	failure := store.PersonalCDNCheck{At: now, Fingerprint: "first", Message: "failed"}
	saved := recordPersonalCDNVerdict(&pool, "entry", "104.20.9.9", failure)
	if !saved.Failed {
		t.Fatal("failure not recorded")
	}
	partial := store.PersonalCDNCheck{At: now.Add(time.Second), Fingerprint: "first", Partial: true}
	saved = recordPersonalCDNVerdict(&pool, "entry", "104.20.9.9", partial)
	if !saved.Failed {
		t.Fatal("partial cleared known failure")
	}
	success := store.PersonalCDNCheck{At: now.Add(2 * time.Second), Fingerprint: "first", OK: true}
	recordPersonalCDNVerdict(&pool, "entry", "104.20.9.9", success)
	saved = recordPersonalCDNVerdict(&pool, "entry", "104.20.9.9", failure)
	if !saved.OK || saved.Failed {
		t.Fatal("older in-flight failure overwrote a newer success")
	}
	changed := store.PersonalCDNCheck{At: now.Add(3 * time.Second), Fingerprint: "changed", Partial: true}
	if recordPersonalCDNVerdict(&pool, "entry", "104.20.9.9", changed).Failed {
		t.Fatal("unrelated context inherited failure")
	}
}
