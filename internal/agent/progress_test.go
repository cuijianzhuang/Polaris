package agent

import (
	"errors"
	"strings"
	"sync"
	"testing"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
)

func TestProgressTracker(t *testing.T) {
	var mu sync.Mutex
	var got []*polarisv1.Progress
	p := &progressTracker{send: func(m *polarisv1.Progress) { mu.Lock(); got = append(got, m); mu.Unlock() }}

	p.step("ignored", 0, 0) // no activity: nothing is sent
	end := p.begin(7, "应用配置")
	inner := p.begin(0, "安装") // joins the running activity
	r := &progressReader{r: strings.NewReader(strings.Repeat("x", 1000)), total: 1000, stage: "下载 xray 1.0", p: p}
	buf := make([]byte, 100)
	for {
		if _, err := r.Read(buf); err != nil {
			break
		}
	}
	inner(nil)
	if s := p.snapshot(); s == nil || !s.Active || s.Rev != 7 {
		t.Fatalf("activity ended by the inner end: %+v", s)
	}
	end(errors.New("boom"))
	if p.snapshot() != nil {
		t.Fatal("activity still running after end")
	}

	if len(got) < 4 {
		t.Fatalf("too few messages: %d", len(got))
	}
	var sawDone bool
	for _, m := range got[:len(got)-1] {
		if !m.Active || m.Rev != 7 || m.StartedAt == 0 {
			t.Fatalf("bad running message: %+v", m)
		}
		if m.Stage == "下载 xray 1.0" && m.Done == 1000 && m.Total == 1000 {
			sawDone = true // the last chunk is always sent despite throttling
		}
	}
	if !sawDone {
		t.Fatal("final byte count not reported")
	}
	if n := len(got); n > 8 {
		t.Fatalf("byte updates not throttled: %d messages", n)
	}
	last := got[len(got)-1]
	if last.Active || last.Error != "boom" || last.Rev != 7 {
		t.Fatalf("bad final message: %+v", last)
	}
}
