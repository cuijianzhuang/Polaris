package panel

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestSiteIconsOnPanelAndSubscriptionHosts(t *testing.T) {
	s := newTestServer(t)
	s.cfg.PublicURL = "https://panel.example.com"
	s.cfg.AdminPath = "/hidden"
	s.st.SetSetting("sub_base_url", "https://sub.example.com")
	resetSubHostCache()
	t.Cleanup(resetSubHostCache)
	icon := []byte("\x89PNG\r\n\x1a\nsite-icon")
	s.webFS = http.FS(fstest.MapFS{"apple-touch-icon.png": &fstest.MapFile{Data: icon}})
	h := s.Router()

	for _, host := range []string{"panel.example.com", "sub.example.com"} {
		for _, path := range []string{"/favicon.ico?v=new", "/apple-touch-icon.png?v=new"} {
			r := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), icon) || w.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("%s%s: status=%d, type=%s, bytes=%d", host, path, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
			}
			etag := w.Header().Get("ETag")
			if etag == "" {
				t.Fatal("missing content ETag")
			}
			r.Header.Set("If-None-Match", etag)
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
				t.Fatalf("conditional icon request: status=%d, bytes=%d", w.Code, w.Body.Len())
			}
		}
	}

	// A strict subscription host must continue hiding the admin UI.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://sub.example.com/hidden/", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("admin UI exposed on subscription host: %d", w.Code)
	}

	// API-only deployments without embedded assets must fail gracefully.
	s.webFS = nil
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://panel.example.com/favicon.ico", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("icon without embedded assets: %d", w.Code)
	}
}
