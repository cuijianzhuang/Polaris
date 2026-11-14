package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRunJobSkipsOverlapAndRecovers(t *testing.T) {
	s := newTestServer(t)
	release, started := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runJob("slow", func() { close(started); <-release })
	}()
	<-started
	if s.runJob("slow", func() { t.Error("overlapping run started") }) {
		t.Fatal("second run reported as started")
	}
	close(release)
	wg.Wait()
	if !s.runJob("slow", func() {}) {
		t.Fatal("run after the previous finished was skipped")
	}
	if s.runJob("boom", func() { panic("x") }); s.jobs.get("boom").busy.Load() {
		t.Fatal("panicking job stayed busy")
	}
	st := s.jobs.get("slow")
	if st.runs.Load() != 2 || st.skips.Load() != 1 || s.jobs.get("boom").panics.Load() != 1 {
		t.Fatalf("runs=%d skips=%d panics=%d", st.runs.Load(), st.skips.Load(), s.jobs.get("boom").panics.Load())
	}
	w := &promWriter{}
	s.writeJobMetrics(w)
	if out := w.b.String(); !strings.Contains(out, `polaris_job_skipped_total{job="slow"} 1`) || !strings.Contains(out, `polaris_job_panics_total{job="boom"} 1`) {
		t.Fatalf("metrics:\n%s", out)
	}
}

func TestHealthProbesOnlyForLocalPeers(t *testing.T) {
	s := newTestServer(t)
	h := s.Router()
	try := func(path, remote string, hdr map[string]string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = remote
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		if code, body := try(p, "127.0.0.1:5000", nil); code != 200 || body != "ok" {
			t.Fatalf("%s loopback: %d %q", p, code, body)
		}
		if code, _ := try(p, "10.0.0.5:5000", nil); code != 200 {
			t.Fatalf("%s LAN: %d", p, code)
		}
		if code, body := try(p, "203.0.113.9:5000", nil); code != 404 || body == "ok" {
			t.Fatalf("%s public: %d %q", p, code, body)
		}
		if code, body := try(p, "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "203.0.113.9"}); code != 404 || body == "ok" {
			t.Fatalf("%s via local reverse proxy: %d %q", p, code, body)
		}
	}
}
