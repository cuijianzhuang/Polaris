package core

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestProcessStartCancellationLeavesNoChild(t *testing.T) {
	for _, during := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		p := NewProcess("canceled-start", "/bin/sleep", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), "30")
		p.BindToParent = ParentBoundSupported()
		if during {
			timer := time.AfterFunc(50*time.Millisecond, cancel)
			defer timer.Stop()
		} else {
			cancel()
		}
		err := p.Start(ctx)
		cancel()
		defer p.Stop(context.Background())
		if err == nil || p.Running() || p.PID() != 0 {
			t.Fatal("canceled startup left a process able to accept temporary credentials")
		}
	}
}
