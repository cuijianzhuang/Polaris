package agent

import (
	"io"
	"sync"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
)

// progressTracker reports the agent's current activity to the panel: one
// activity at a time (applies are serialised), steps sent as they change,
// byte counts at most every progressEvery.
type progressTracker struct {
	mu    sync.Mutex
	depth int
	cur   *polarisv1.Progress
	last  time.Time
	send  func(*polarisv1.Progress)
}

const progressEvery = 700 * time.Millisecond

// begin starts an activity, or joins the running one (an install inside an
// apply) and returns the function that ends it.
func (p *progressTracker) begin(rev int64, stage string) func(error) {
	p.mu.Lock()
	p.depth++
	if p.depth == 1 {
		p.cur = &polarisv1.Progress{Active: true, Rev: rev, StartedAt: time.Now().Unix()}
	}
	p.mu.Unlock()
	p.step(stage, 0, 0)
	return p.end
}

func (p *progressTracker) end(err error) {
	p.mu.Lock()
	if p.depth == 0 {
		p.mu.Unlock()
		return
	}
	p.depth--
	if p.depth > 0 {
		p.mu.Unlock()
		return
	}
	m := &polarisv1.Progress{Active: false, Rev: p.cur.Rev, StartedAt: p.cur.StartedAt, Stage: p.cur.Stage}
	if err != nil {
		m.Error = err.Error()
	}
	p.cur = nil
	p.mu.Unlock()
	p.emit(m)
}

// step names the current step; done/total count bytes (total 0 = unknown).
func (p *progressTracker) step(stage string, done, total int64) {
	p.mu.Lock()
	if p.cur == nil {
		p.mu.Unlock()
		return
	}
	changed := p.cur.Stage != stage
	if !changed && time.Since(p.last) < progressEvery && (total == 0 || done < total) {
		p.cur.Done, p.cur.Total = done, total
		p.mu.Unlock()
		return
	}
	p.cur.Stage, p.cur.Done, p.cur.Total = stage, done, total
	p.last = time.Now()
	m := cloneProgress(p.cur)
	p.mu.Unlock()
	p.emit(m)
}

// snapshot is the running activity (nil when idle), resent on reconnect.
func (p *progressTracker) snapshot() *polarisv1.Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur == nil {
		return nil
	}
	return cloneProgress(p.cur)
}

func (p *progressTracker) emit(m *polarisv1.Progress) {
	if p.send != nil {
		p.send(m)
	}
}

func cloneProgress(m *polarisv1.Progress) *polarisv1.Progress {
	return &polarisv1.Progress{Active: m.Active, Stage: m.Stage, Done: m.Done, Total: m.Total, Rev: m.Rev, StartedAt: m.StartedAt, Error: m.Error}
}

// progressReader reports bytes read under a step name.
type progressReader struct {
	r     io.Reader
	n     int64
	total int64
	stage string
	p     *progressTracker
}

func (r *progressReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.n += int64(n)
	if r.p != nil {
		r.p.step(r.stage, r.n, r.total)
	}
	return n, err
}
