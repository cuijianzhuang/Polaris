package singbox

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

type statsWireCodec struct{}

func (statsWireCodec) Name() string { return "proto" }
func (statsWireCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("unexpected %T", v)
	}
	return *b, nil
}
func (statsWireCodec) Unmarshal(b []byte, v any) error {
	p, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("unexpected %T", v)
	}
	*p = append((*p)[:0], b...)
	return nil
}

// Exercise the real CollectTraffic gRPC request against a stats service that
// implements sing-box's counter.Swap(0) semantics, including service fallback.
func TestCollectTrafficReadAndReset(t *testing.T) {
	var mu sync.Mutex
	counts := [2]int64{100, 200}
	resetRequests := 0
	srv := grpc.NewServer(grpc.ForceServerCodec(statsWireCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		if method != "/v2ray.core.app.stats.command.StatsService/QueryStats" {
			return status.Error(codes.Unimplemented, "try legacy service name")
		}
		var req []byte
		if err := stream.RecvMsg(&req); err != nil {
			return err
		}
		reset := false
		if err := core.PBRange(req, func(n protowire.Number, typ protowire.Type, _ []byte, v uint64) {
			if n == 2 && typ == protowire.VarintType {
				reset = v == 1
			}
		}); err != nil {
			return err
		}
		mu.Lock()
		v := counts
		if reset {
			counts = [2]int64{}
			resetRequests++
		}
		mu.Unlock()
		var resp []byte
		for i, name := range []string{"user>>>u>>>traffic>>>uplink", "user>>>u>>>traffic>>>downlink"} {
			stat := core.PBString(nil, 1, name)
			stat = core.PBVarint(stat, 2, uint64(v[i]))
			resp = core.PBBytes(resp, 1, stat)
		}
		return stream.SendMsg(&resp)
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(listener)
	t.Cleanup(srv.Stop)
	ctx := context.Background()
	a := New(core.Options{APIListen: listener.Addr().String()})
	t.Cleanup(func() {
		if a.api != nil {
			_ = a.api.Close()
		}
	})
	a.proc = core.NewProcess("fake-singbox", "/bin/sh", t.TempDir(), slog.Default(), "-c", "trap '' HUP; while :; do sleep 1; done")
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(ctx) })
	read := func(adapter *Adapter, up, down int64) {
		t.Helper()
		ds, err := adapter.CollectTraffic(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if up == 0 && down == 0 {
			if len(ds) != 0 {
				t.Fatalf("idle replay: %v", ds)
			}
			return
		}
		if len(ds) != 1 || ds[0].UserID != "u" || ds[0].Up != up || ds[0].Down != down {
			t.Fatalf("got %v want %d/%d", ds, up, down)
		}
	}
	read(a, 100, 200)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	read(a, 0, 0) // A live reload / idempotent start must never replay history.
	mu.Lock()
	counts = [2]int64{5, 9}
	mu.Unlock()
	read(a, 5, 9)
	// Recreating the collector while the stats service stays alive must not
	// make its lifetime totals reappear, even without any in-memory baseline.
	fresh := New(core.Options{APIListen: listener.Addr().String()})
	t.Cleanup(func() {
		if fresh.api != nil {
			_ = fresh.api.Close()
		}
	})
	read(fresh, 0, 0)
	mu.Lock()
	counts = [2]int64{300, 400}
	mu.Unlock()
	read(fresh, 300, 400)
	mu.Lock()
	defer mu.Unlock()
	if resetRequests != 5 {
		t.Fatalf("read-and-reset requests = %d", resetRequests)
	}
}
