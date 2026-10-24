package core

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestProcessStopDrainsFinalLogCallbacks(t *testing.T) {
	final := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p := NewProcess("log-drain", "/bin/sh", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), "-c", "trap 'echo final; exit 0' TERM; echo ready; while :; do sleep 0.1; done")
	p.Filter = func(line string) bool {
		if line == "final" {
			close(final)
			<-release
		}
		return true
	}
	t.Cleanup(func() { once.Do(func() { close(release) }); p.Stop(context.Background()) })
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { p.Stop(context.Background()); close(stopped) }()
	select {
	case <-final:
	case <-time.After(3 * time.Second):
		t.Fatal("final log not reached")
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned while old process log callback was still pending")
	case <-time.After(30 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not complete after log drain")
	}
}
