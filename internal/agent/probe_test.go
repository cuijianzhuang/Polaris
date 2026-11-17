package agent

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// serveTCP accepts and immediately closes connections, so dials succeed.
func serveTCP(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().String(), func() {
		close(done)
		ln.Close()
	}
}

// closedTCP returns an address nothing listens on.
func closedTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestProbeCountSamples(t *testing.T) {
	addr, stop := serveTCP(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := probe(ctx, map[string]string{"tcp": addr, "count": "5"})
	if err != nil {
		t.Fatal(err)
	}
	var s probeSample
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("sample is not JSON: %q (%v)", out, err)
	}
	if s.Sent != 5 || s.Lost != 0 {
		t.Fatalf("sample: %+v", s)
	}
}

func TestProbeCountLoss(t *testing.T) {
	addr := closedTCP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := probe(ctx, map[string]string{"tcp": addr, "count": "5"})
	if err != nil {
		t.Fatalf("all-lost sample must not be an error: %v", err)
	}
	var s probeSample
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("sample is not JSON: %q (%v)", out, err)
	}
	if s.Sent != 5 || s.Lost != 5 || s.Ms != 0 || s.Min != 0 || s.Max != 0 {
		t.Fatalf("sample: %+v", s)
	}
}

func TestProbeCountClamped(t *testing.T) {
	addr, stop := serveTCP(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := probe(ctx, map[string]string{"tcp": addr, "count": "99"})
	if err != nil {
		t.Fatal(err)
	}
	var s probeSample
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("sample is not JSON: %q (%v)", out, err)
	}
	if s.Sent != 10 || s.Lost != 0 {
		t.Fatalf("count must clamp to 10: %+v", s)
	}
}

func TestProbeSingleUnchanged(t *testing.T) {
	addr, stop := serveTCP(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// No count: the historical plain-integer output.
	out, err := probe(ctx, map[string]string{"tcp": addr})
	if err != nil {
		t.Fatal(err)
	}
	var ms int
	if err := json.Unmarshal([]byte(out), &ms); err != nil {
		t.Fatalf("single probe must stay a plain integer: %q (%v)", out, err)
	}
	// And a failed single dial is still an error, not a sample.
	if _, err := probe(ctx, map[string]string{"tcp": closedTCP(t)}); err == nil {
		t.Fatal("failed single probe must error")
	}
}
