package panel

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRelayAssetCacheCompleteness(t *testing.T) {
	const limit = 8
	for _, tc := range []struct {
		name     string
		body     string
		declared int // -1 forces a chunked response
		cached   bool
		status   int
	}{
		{"chunked below limit", "abc", -1, true, http.StatusOK},
		{"chunked exact limit", "12345678", -1, true, http.StatusOK},
		{"chunked over limit", "123456789", -1, false, http.StatusOK},
		{"known exact limit", "12345678", limit, true, http.StatusOK},
		{"known over limit", "123456789", limit + 1, false, http.StatusBadGateway},
		{"incomplete body", "abc", limit, false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			var hits atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if tc.declared >= 0 {
					w.Header().Set("Content-Length", strconv.Itoa(tc.declared))
				} else {
					w.(http.Flusher).Flush()
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(up.Close)
			oldWeb := githubWeb
			githubWeb = up.URL
			t.Cleanup(func() { githubWeb = oldWeb })
			asset := "https://github.com/XTLS/Xray-core/releases/download/v-cache-test/core.zip"
			for i := 0; i < 2; i++ {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/agent/core?u="+url.QueryEscape(asset), nil)
				s.relayCoreAsset(w, r, asset, limit)
				if w.Code != tc.status {
					t.Fatalf("request %d: status=%d, want %d", i, w.Code, tc.status)
				}
				if tc.cached && w.Body.String() != tc.body {
					t.Fatalf("request %d: incomplete payload %q", i, w.Body.String())
				}
				if tc.declared < 0 && w.Body.Len() > limit {
					t.Fatal("forwarded bytes beyond relay limit")
				}
			}
			wantHits, wantFiles := int32(2), 0
			if tc.cached {
				wantHits, wantFiles = 1, 1
			}
			if n := hits.Load(); n != wantHits {
				t.Fatalf("upstream hits=%d, want %d", n, wantHits)
			}
			files, err := os.ReadDir(filepath.Join(s.downloadsDir(), "cores"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(files) != wantFiles {
				t.Fatalf("cache contains %d files, want %d: %v", len(files), wantFiles, files)
			}
			for _, f := range files {
				if strings.HasPrefix(f.Name(), ".dl-") {
					t.Fatal("temporary download leaked")
				}
			}
		})
	}
}

type relayFailingReader struct{ err error }

func (r relayFailingReader) Read([]byte) (int, error) { return 0, r.err }

func TestRelayAssetBoundaryReadError(t *testing.T) {
	failure := errors.New("upstream failed at boundary")
	src := io.MultiReader(strings.NewReader("12345678"), relayFailingReader{failure})
	var dst bytes.Buffer
	n, err := copyRelayAsset(&dst, src, 8)
	if n != 8 || !errors.Is(err, failure) || dst.String() != "12345678" {
		t.Fatalf("boundary error lost: n=%d err=%v body=%q", n, err, dst.String())
	}
}

func TestRelayTarget(t *testing.T) {
	for u, want := range map[string]bool{
		"https://github.com/XTLS/Xray-core/releases/download/v26.3.27/Xray-linux-64.zip":     true,
		"https://github.com/apernet/hysteria/releases/download/app/v2.6.2/hashes.txt":        true,
		"https://api.github.com/repos/SagerNet/sing-box/releases/tags/v1.13.5":               true,
		"https://github.com/evil/repo/releases/download/v1/x":                                false,
		"https://github.com/XTLS/Xray-core/archive/refs/heads/main.zip":                      false,
		"https://github.com/XTLS/Xray-core/releases/download/../../../../evil/x":             false,
		"https://api.github.com/repos/SagerNet/sing-box/releases":                            false,
		"https://example.com/XTLS/Xray-core/releases/download/v1/x":                          false,
		"https://github.com/XTLS/Xray-core/releases/download/v1/x?redirect=https://evil.com": false,
	} {
		if _, ok := relayTarget(u); ok != want {
			t.Errorf("relayTarget(%q) = %v, want %v", u, ok, want)
		}
	}
}

func TestRelayCachesReleases(t *testing.T) {
	s := newTestServer(t)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/XTLS/Xray-core/releases/download/v1.0.0/Xray-linux-64.zip":
			w.Write([]byte("zipdata"))
		case "/repos/XTLS/Xray-core/releases/tags/v1.0.0":
			w.Write([]byte(`{"assets":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	githubAPI, githubWeb = up.URL, up.URL
	defer func() { githubAPI, githubWeb = "https://api.github.com", "https://github.com" }()

	get := func(u string) (int, string) {
		w := httptest.NewRecorder()
		s.relayCore(w, httptest.NewRequest(http.MethodGet, "/agent/core?u="+url.QueryEscape(u), nil))
		return w.Code, w.Body.String()
	}
	asset := "https://github.com/XTLS/Xray-core/releases/download/v1.0.0/Xray-linux-64.zip"
	for i := 0; i < 2; i++ {
		if code, body := get(asset); code != 200 || body != "zipdata" {
			t.Fatalf("asset #%d: %d %q", i, code, body)
		}
	}
	api := "https://api.github.com/repos/XTLS/Xray-core/releases/tags/v1.0.0"
	for i := 0; i < 2; i++ {
		if code, body := get(api); code != 200 || body != `{"assets":[]}` {
			t.Fatalf("api #%d: %d %q", i, code, body)
		}
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("upstream hit %d times, want 2 (both cached)", n)
	}
	if code, _ := get("https://github.com/XTLS/Xray-core/releases/download/v9/missing.zip"); code != http.StatusBadGateway {
		t.Errorf("missing asset: %d", code)
	}
	if code, _ := get("https://github.com/evil/x/releases/download/v1/a"); code != http.StatusBadRequest {
		t.Errorf("foreign repo: %d", code)
	}
}
