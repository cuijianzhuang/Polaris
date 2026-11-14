package panel

import (
	"context"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// jobStat tracks one scheduled job on this instance. A job never overlaps
// with itself: a tick that finds the previous run still going is skipped
// (and counted) instead of starting a second copy that would repeat
// external calls, probes or notifications.
type jobStat struct {
	busy        atomic.Bool
	runs        atomic.Int64
	skips       atomic.Int64
	panics      atomic.Int64
	lastStart   atomic.Int64 // unix nanoseconds
	lastSeconds atomic.Uint64
	lastOK      atomic.Int64 // unix nanoseconds of the last run that did not panic
}

type jobRegistry struct{ m sync.Map } // name -> *jobStat

func (r *jobRegistry) get(name string) *jobStat {
	v, _ := r.m.LoadOrStore(name, &jobStat{})
	return v.(*jobStat)
}

func (r *jobRegistry) names() []string {
	var out []string
	r.m.Range(func(k, _ any) bool { out = append(out, k.(string)); return true })
	sort.Strings(out)
	return out
}

// runJob runs fn as the named job unless its previous run is still going.
// It reports whether fn ran. Panics are recovered and counted.
func (s *Server) runJob(name string, fn func()) bool {
	st := s.jobs.get(name)
	if !st.busy.CompareAndSwap(false, true) {
		st.skips.Add(1)
		s.log.Warn("job still running, skipped this tick", "job", name)
		return false
	}
	start := time.Now()
	st.lastStart.Store(start.UnixNano())
	st.runs.Add(1)
	ok := false
	defer func() {
		if r := recover(); r != nil {
			st.panics.Add(1)
			s.log.Error("job panicked", "job", name, "panic", r)
		}
		st.lastSeconds.Store(uint64(time.Since(start).Microseconds()))
		if ok {
			st.lastOK.Store(time.Now().UnixNano())
		}
		st.busy.Store(false)
	}()
	fn()
	ok = true
	return true
}

// writeJobMetrics adds per-job series for this instance to a scrape.
func (s *Server) writeJobMetrics(w *promWriter) {
	now := time.Now()
	for _, name := range s.jobs.names() {
		st := s.jobs.get(name)
		w.gauge("polaris_job_runs_total", "Scheduled job runs started on this instance.", float64(st.runs.Load()), "job", name)
		w.gauge("polaris_job_skipped_total", "Ticks skipped because the previous run was still going.", float64(st.skips.Load()), "job", name)
		w.gauge("polaris_job_panics_total", "Scheduled job runs that panicked.", float64(st.panics.Load()), "job", name)
		w.gauge("polaris_job_last_duration_seconds", "Duration of the last finished run.", float64(st.lastSeconds.Load())/1e6, "job", name)
		running := 0.0
		if st.busy.Load() {
			running = 1
		}
		w.gauge("polaris_job_running", "1 while the job is running on this instance.", running, "job", name)
		if ok := st.lastOK.Load(); ok > 0 {
			w.gauge("polaris_job_last_success_age_seconds", "Seconds since the last run that finished without panicking.", now.Sub(time.Unix(0, ok)).Seconds(), "job", name)
		}
	}
}

// probeGate answers health probes only to direct peers on loopback or a
// private network (Docker HEALTHCHECK, systemd, a load balancer on the LAN).
// Anyone else gets the decoy, so the public site does not reveal a panel.
// The peer address is used as-is: a forwarded header cannot open it,
// it can only close it.
func (s *Server) probeGate(h gin.HandlerFunc) gin.HandlerFunc {
	decoy := s.decoy()
	return func(c *gin.Context) {
		host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		ip := net.ParseIP(host)
		// A reverse proxy on the same host (Caddy, Nginx) connects from
		// loopback for every public visitor: proxied requests stay closed.
		proxied := c.GetHeader("X-Forwarded-For") != "" || c.GetHeader("Forwarded") != "" || c.GetHeader("X-Real-IP") != ""
		if err != nil || ip == nil || proxied || !(ip.IsLoopback() || ip.IsPrivate()) {
			decoy(c)
			return
		}
		h(c)
	}
}

// healthz answers 200 while the process serves HTTP (liveness).
func (s *Server) healthz(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.String(http.StatusOK, "ok")
}

// readyz answers 200 when the database answers (readiness), 503 otherwise.
// It reveals nothing beyond the status.
func (s *Server) readyz(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	db, err := s.st.DB.DB()
	if err == nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		err = db.PingContext(ctx)
		cancel()
	}
	if err != nil {
		c.String(http.StatusServiceUnavailable, "database unavailable")
		return
	}
	c.String(http.StatusOK, "ok")
}
