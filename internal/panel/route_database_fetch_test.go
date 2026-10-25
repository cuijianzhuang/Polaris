package panel

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type routeDBRoundTrip func(*http.Request) (*http.Response, error)

func (f routeDBRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func repoResponse(req *http.Request, raw []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw)), ContentLength: int64(len(raw)), Request: req}
}
func repoSum(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func repoCityGzip(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, e := w.Write(geoFixture(t)); e != nil {
		t.Fatal(e)
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}

func TestRepositoryDatabasePresetsAndURLGuard(t *testing.T) {
	for _, raw := range []string{"http://raw.githubusercontent.com/Loyalsoldier/geoip/release/GeoLite2-ASN.mmdb", "https://127.0.0.1/file", "https://github.com.evil.test/lwpk110/free-geoip-databases/releases/download/x/file", "https://user:pass@github.com/lwpk110/free-geoip-databases/releases/download/x/file", "https://raw.githubusercontent.com:8443/Loyalsoldier/geoip/release/GeoLite2-ASN.mmdb", "https://raw.githubusercontent.com/other/repo/file"} {
		u, _ := url.Parse(raw)
		if validDatabaseRepositoryURL(u) {
			t.Fatal(raw)
		}
	}
	for _, kind := range []string{"asn", "geo"} {
		if _, _, ok := repositoryPreset(kind, "https://127.0.0.1/database"); ok {
			t.Fatal("arbitrary URL accepted")
		}
	}
	calls := 0
	client := newRepositoryDatabaseClient(routeDBRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		r := repoResponse(req, nil)
		r.StatusCode = 302
		r.Header.Set("Location", "https://127.0.0.1/private")
		return r, nil
	}))
	if _, err := repositoryBytes(context.Background(), client, "https://raw.githubusercontent.com/Loyalsoldier/geoip/release/GeoLite2-ASN.mmdb", 128); err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
	// A wrong Content-Length cannot bypass the streaming size limit.
	client = newRepositoryDatabaseClient(routeDBRoundTrip(func(req *http.Request) (*http.Response, error) {
		r := repoResponse(req, []byte("too big"))
		r.ContentLength = -1
		return r, nil
	}))
	if _, err := repositoryBytes(context.Background(), client, "https://raw.githubusercontent.com/Loyalsoldier/geoip/release/GeoLite2-ASN.mmdb", 3); !errors.Is(err, errRepositorySize) {
		t.Fatal(err)
	}
}
func TestRepositoryDatabaseInstallAndCAS(t *testing.T) {
	s, c := routeScheduleFixture(t)
	raw := asnFixture(t)
	calls := 0
	s.routeDatabaseTransport = routeDBRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Fatal("panel credentials leaked")
		}
		data := raw
		if strings.HasSuffix(req.URL.Path, ".sha256sum") {
			data = []byte(repoSum(raw) + "  GeoLite2-ASN.mmdb")
		}
		return repoResponse(req, data), nil
	})
	st, out := c.do("POST", "/tools/route-asn/database/fetch", map[string]any{"source": "loyalsoldier", "expectedRevision": 0})
	if st != 200 || out["effectiveEnabled"] != false || out["policy"].(map[string]any)["sha256"] != repoSum(raw) || calls != 2 {
		t.Fatal(st, out, calls)
	}
	// Stale revisions fail before making another upstream request.
	if st, out := c.do("POST", "/tools/route-asn/database/fetch", map[string]any{"source": "loyalsoldier", "expectedRevision": 0}); st != 409 || calls != 2 {
		t.Fatal(st, out, calls)
	}
	if st, out := c.do("POST", "/tools/route-asn/database/fetch", map[string]any{"source": "wp-statistics", "expectedRevision": 1}); st != 400 || calls != 2 {
		t.Fatal(st, out, calls)
	}
	if st, out := c.do("POST", "/tools/route-asn/database/fetch", map[string]any{"source": "loyalsoldier"}); st != 400 {
		t.Fatal(st, out)
	}
	// An administrator changing the policy while the download runs wins the CAS.
	s.routeDatabaseTransport = routeDBRoundTrip(func(req *http.Request) (*http.Response, error) {
		data := []byte(repoSum(raw))
		if !strings.HasSuffix(req.URL.Path, ".sha256sum") {
			if st, out := c.do("PUT", "/tools/route-asn", map[string]any{"enabled": true, "expectedRevision": 1}); st != 200 {
				t.Fatal(st, out)
			}
			data = raw
		}
		return repoResponse(req, data), nil
	})
	if st, out := c.do("POST", "/tools/route-asn/database/fetch", map[string]any{"source": "loyalsoldier", "expectedRevision": 1}); st != 409 {
		t.Fatal(st, out)
	}
	p, err := readRouteASNPolicy(s.st.DB)
	if err != nil || !p.Enabled || p.Revision != 2 || p.SHA256 != repoSum(raw) {
		t.Fatal(p, err)
	}
}
func TestRepositoryCityGzipAndReleaseSelection(t *testing.T) {
	s, c := routeScheduleFixture(t)
	raw := repoCityGzip(t)
	s.routeDatabaseTransport = routeDBRoundTrip(func(req *http.Request) (*http.Response, error) { return repoResponse(req, raw), nil })
	if st, out := c.do("POST", "/tools/route-geo/database/fetch", map[string]any{"source": "wp-statistics", "expectedRevision": 0}); st != 200 || out["effectiveEnabled"] != false || out["policy"].(map[string]any)["sha256"] != repoSum(geoFixture(t)) {
		t.Fatal(st, out)
	}
	assetURL := "https://github.com/lwpk110/free-geoip-databases/releases/download/geolite2-20261023/GeoLite2-City-20261023.mmdb"
	metadata := []map[string]any{
		{"tag_name": "dbip-20261024", "assets": []map[string]any{{"name": "dbip-city.mmdb", "browser_download_url": "https://evil.test/file"}}},
		{"tag_name": "geolite2-20261023", "assets": []map[string]any{{"name": "GeoLite2-ASN-20261023.mmdb", "browser_download_url": "https://evil.test/file"}, {"name": "GeoLite2-City-20261023.mmdb", "browser_download_url": assetURL, "digest": "sha256:" + repoSum(geoFixture(t))}}},
	}
	body, _ := json.Marshal(metadata)
	calls := 0
	s.routeDatabaseTransport = routeDBRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		switch req.URL.String() {
		case routeDatabaseReleases:
			return repoResponse(req, body), nil
		case assetURL:
			return repoResponse(req, geoFixture(t)), nil
		default:
			t.Fatal("unexpected URL", req.URL)
			return nil, errors.New("unexpected URL")
		}
	})
	if st, out := c.do("POST", "/tools/route-geo/database/fetch", map[string]any{"source": "free-geoip", "expectedRevision": 1}); st != 200 || calls != 2 || out["policy"].(map[string]any)["revision"] != float64(1) {
		t.Fatal(st, out, calls)
	}
}
func TestRepositoryDatabaseFailurePreservesInstalled(t *testing.T) {
	s, c := routeScheduleFixture(t)
	hash := installASN(t, s, c)
	s.routeDatabaseTransport = routeDBRoundTrip(func(req *http.Request) (*http.Response, error) {
		data := asnFixture(t)
		if strings.HasSuffix(req.URL.Path, ".sha256sum") {
			data = []byte(strings.Repeat("0", 64))
		}
		return repoResponse(req, data), nil
	})
	if st, out := c.do("POST", "/tools/route-asn/database/fetch", map[string]any{"source": "loyalsoldier", "expectedRevision": 2}); st != 502 {
		t.Fatal(st, out)
	}
	p, err := readRouteASNPolicy(s.st.DB)
	if err != nil || p.SHA256 != hash || !p.Enabled || p.Revision != 2 {
		t.Fatal(p, err)
	}
	s.routeDatabaseTransport = routeDBRoundTrip(func(req *http.Request) (*http.Response, error) {
		r := repoResponse(req, []byte("rate limited"))
		r.StatusCode = 403
		return r, nil
	})
	if st, out := c.do("POST", "/tools/route-asn/database/fetch", map[string]any{"source": "loyalsoldier", "expectedRevision": 2}); st != 502 {
		t.Fatal(st, out)
	}
	// Cancellation must stop the request instead of installing partial data.
	client := newRepositoryDatabaseClient(routeDBRoundTrip(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := downloadRepositoryDatabase(ctx, client, "asn", "loyalsoldier"); err == nil {
		t.Fatal("cancelled download succeeded")
	}
}
func TestRepositoryDatabaseAuthorization(t *testing.T) {
	_, c := routeScheduleFixture(t)
	st, out := c.do("POST", "/tokens", map[string]any{"name": "read-only", "scopes": []string{ScopeNodesRead}})
	if st != 201 {
		t.Fatal(st, out)
	}
	for _, client := range []*apiClient{{t: t, base: c.base}, {t: t, base: c.base, tok: out["secret"].(string)}} {
		want := 401
		if client.tok != "" {
			want = 403
		}
		for _, kind := range []string{"asn", "geo"} {
			if st, out := client.do("POST", "/tools/route-"+kind+"/database/fetch", map[string]any{"source": "free-geoip", "expectedRevision": 0}); st != want {
				t.Fatal(st, out)
			}
		}
	}
}

func TestRepositoryProxyEndpoint(t *testing.T) {
	for raw, want := range map[string]string{"http://localhost": "localhost:80", "https://proxy.example": "proxy.example:443", "http://[::1]:7890": "[::1]:7890", "socks5://localhost": "localhost:1080"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := databaseProxyAddress(u); got != want {
			t.Fatal(got, want)
		}
	}
}
