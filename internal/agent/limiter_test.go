package agent

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestLimiterStats(t *testing.T) {
	l := newLimiter(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.setLimits([]model.UserLimit{{User: "u1", UpKbps: 8000, DownKbps: 16000}, {User: "u2", DownKbps: 1000}})
	if st := l.stats(); len(st) != 2 || st[0].UpKbps != 8000 || st[0].DownKbps != 16000 || st[1].UpKbps != 0 || st[0].DownBps != 0 {
		t.Fatalf("first stats %+v", st)
	}
	time.Sleep(200 * time.Millisecond)
	l.buckets["u1"].downBytes.Add(250_000) // 2 Mbit in ~0.2 s ≈ 10 Mbps
	st := l.stats()
	if st[0].DownBps < 5_000_000 || st[0].DownBps > 12_000_000 || st[0].UpBps != 0 {
		t.Fatalf("speed %+v", st[0])
	}
	// A user no longer limited drops out of the report.
	l.setLimits([]model.UserLimit{{User: "u2", DownKbps: 1000}})
	if st := l.stats(); len(st) != 1 || st[0].User != "u2" {
		t.Fatalf("after removal %+v", st)
	}
}
