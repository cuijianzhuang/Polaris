package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestRouteTraceEvidence(t *testing.T) {
	req := model.RouteTraceRequest{IPv4: "1.1.1.1", IPv6: "2606:4700:4700::1111"}
	results, err := collectRouteTrace(t.Context(), req, func(ctx context.Context, family, target string) (string, error) {
		if family == "6" {
			return " 1 *\n", exec.ErrNotFound
		}
		return "traceroute to 1.1.1.1\n 1 192.168.1.1 1.2 ms\n 2 *\n 3 1.1.1.1 3.1 ms\n", nil
	})
	if err != nil || len(results) != 2 {
		t.Fatal(results, err)
	}
	if results[0].State != "reached" || len(results[0].Hops) != 3 || len(results[0].Hops[1].Addresses) != 0 || results[0].CheckedAt.IsZero() {
		t.Fatal(results[0])
	}
	if results[1].State != "unavailable" {
		t.Fatal(results[1])
	}
}

func TestRouteTraceFailureAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		err   error
		state string
	}{{errors.New("permission denied"), "failed"}, {nil, "partial"}} {
		results, err := collectRouteTrace(t.Context(), model.RouteTraceRequest{IPv4: "1.1.1.1"}, func(context.Context, string, string) (string, error) { return " 1 10.0.0.1 1 ms\n", tc.err })
		if err != nil || results[0].State != tc.state {
			t.Fatal(results, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	results, err := collectRouteTrace(ctx, model.RouteTraceRequest{IPv4: "1.1.1.1"}, func(ctx context.Context, _, _ string) (string, error) { <-ctx.Done(); return " 1 *\n", ctx.Err() })
	if err != nil || results[0].State != "timeout" {
		t.Fatal(results, err)
	}
}

func TestRouteTraceBoundedOutputAndWork(t *testing.T) {
	var b traceOutput
	if n, err := b.Write([]byte(strings.Repeat("x", 100000))); err != nil || n != 100000 || b.Len() != 32768 {
		t.Fatal(n, err, b.Len())
	}
	routeTraceSlots <- struct{}{}
	routeTraceSlots <- struct{}{}
	_, err := collectRouteTrace(t.Context(), model.RouteTraceRequest{IPv4: "1.1.1.1"}, func(context.Context, string, string) (string, error) { t.Error("busy command ran"); return "", nil })
	<-routeTraceSlots
	<-routeTraceSlots
	if err == nil {
		t.Fatal("busy command accepted")
	}
	// Invalid targets never reach an executable.
	_, err = collectRouteTrace(t.Context(), model.RouteTraceRequest{IPv4: "127.0.0.1"}, func(context.Context, string, string) (string, error) { t.Error("invalid command ran"); return "", nil })
	if err == nil {
		t.Fatal("local target accepted")
	}
}

func TestRouteTraceNativeArguments(t *testing.T) {
	// A fixture executable verifies that arguments are passed without a shell,
	// address families are explicit, and traceroute work is bounded.
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	writeTraceFixture(t, dir, "#!/bin/sh\n[ \"$1 $2 $3 $4 $5 $6 $7 $8\" = '-4 -n -q 1 -w 1 -m 20' ] || exit 3\nprintf ' 1 1.1.1.1 1 ms\\n'\n")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	raw, err := runRouteTrace(ctx, "4", "1.1.1.1")
	if err != nil || !strings.Contains(raw, "1.1.1.1") {
		t.Fatal(raw, err)
	}
}

func writeTraceFixture(t *testing.T, dir, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "traceroute"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestRouteTraceUnreachableAndCanonicalIPv6(t *testing.T) {
	_, state := parseRouteTrace(" 1 1.1.1.1 1 ms !H", "1.1.1.1")
	if state == "reached" {
		t.Fatal("unreachable destination marked reached")
	}
	_, state = parseRouteTrace(" 1 2606:4700:4700::1111 1 ms", "2606:4700:4700:0:0:0:0:1111")
	if state != "reached" {
		t.Fatal(state)
	}
}

func TestRouteTraceKillsNativeCommandOnCancellation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	writeTraceFixture(t, dir, "#!/bin/sh\nexec /bin/sleep 30\n")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runRouteTrace(ctx, "4", "1.1.1.1")
	if err == nil || ctx.Err() == nil || time.Since(start) > 2*time.Second {
		t.Fatal(err, ctx.Err(), time.Since(start))
	}
}
