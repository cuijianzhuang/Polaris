package panel

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestBodyLimit(t *testing.T) {
	s, srv, c := newAdminClient(t)
	send := func(method, path string, size int, auth bool, chunked bool) int {
		t.Helper()
		body := append([]byte(`{"pad":"`), append(bytes.Repeat([]byte("a"), size), []byte(`"}`)...)...)
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		if chunked {
			// No Content-Length: the reader limit, not the header check, must stop it.
			req.ContentLength = -1
			req.Body = readCloser{bytes.NewReader(body)}
		}
		req.Header.Set("Content-Type", "application/json")
		if auth {
			req.Header.Set("Authorization", "Bearer "+c.tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	api := s.cfg.APIBase()
	// Unauthenticated endpoints: 1 MiB, with and without Content-Length.
	if code := send("POST", api+"/auth/login", 2<<20, false, false); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("public declared oversize: %d", code)
	}
	if code := send("POST", api+"/auth/login", 2<<20, false, true); code == http.StatusOK {
		t.Fatalf("public chunked oversize accepted: %d", code)
	}
	if code := send("POST", "/sub/nope", 2<<20, false, false); code != http.StatusRequestEntityTooLarge && code != http.StatusNotFound {
		t.Fatalf("public path: %d", code)
	}
	// Authenticated API requests may send more (large imports) but stay bounded.
	if code := send("PUT", api+"/settings", 2<<20, true, false); code == http.StatusRequestEntityTooLarge {
		t.Fatalf("admin 2 MiB rejected: %d", code)
	}
	if code := send("PUT", api+"/settings", 17<<20, true, false); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("admin oversize: %d", code)
	}
	// Uploads with their own limit are not capped by the middleware.
	if code := send("PUT", api+"/tools/route-asn/database", 17<<20, true, false); code == http.StatusRequestEntityTooLarge {
		t.Fatalf("route database upload capped by middleware: %d", code)
	}
	if !strings.HasPrefix(api, "/") {
		t.Fatal(api)
	}
}

type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }
