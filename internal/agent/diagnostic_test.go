package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

type diagnosticTrafficCore struct{ fakeCore }

func (*diagnosticTrafficCore) CollectTraffic(context.Context) ([]model.TrafficDelta, error) {
	return []model.TrafficDelta{{UserID: "u", Up: 1, Down: 2}, {UserID: model.DiagnosticUserPrefix + "test", Up: 10000, Down: 20000}}, nil
}

func TestDiagnosticTrafficNeverQueued(t *testing.T) {
	a := newQueueAgent(t, t.TempDir())
	a.collectFrom(t.Context(), &managed{adapter: &diagnosticTrafficCore{}})
	if len(a.queue) != 1 || len(a.queue[0].Users) != 1 || a.queue[0].Users[0].UserId != "u" || a.queue[0].Users[0].Down != 2 {
		t.Fatal("diagnostic traffic entered durable queue")
	}
}

type cancelDiagnosticCore struct {
	fakeCore
	entered chan struct{}
}

func (f *cancelDiagnosticCore) RunRelayDiagnostic(ctx context.Context, req model.RelayBusinessRequest) (model.RelayBusinessResult, error) {
	close(f.entered)
	<-ctx.Done()
	return model.RelayBusinessResult{State: "failed", CleanupVerified: true}, ctx.Err()
}

func TestRelayDiagnosticCancelAndBusy(t *testing.T) {
	f := &cancelDiagnosticCore{entered: make(chan struct{})}
	a := &Agent{cores: map[model.CoreType]*managed{model.CoreXray: {adapter: f}}}
	request := func(id string) string {
		b, _ := json.Marshal(model.RelayBusinessRequest{ID: id, ExpiresAt: time.Now().Add(15 * time.Second).Unix()})
		return string(b)
	}
	id := strings.Repeat("a", 32)
	done := make(chan error, 1)
	go func() { _, e := a.relayBusinessProbe(context.Background(), request(id)); done <- e }()
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("diagnostic did not start")
	}
	if _, e := a.relayBusinessProbe(t.Context(), request(strings.Repeat("b", 32))); e == nil {
		t.Fatal("concurrent diagnostic accepted")
	}
	if e := a.cancelRelayDiagnostic(t.Context(), id); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("canceled diagnostic succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release worker")
	}
	id = strings.Repeat("c", 32)
	if e := a.cancelRelayDiagnostic(t.Context(), id); e != nil {
		t.Fatal(e)
	}
	if _, e := a.relayBusinessProbe(t.Context(), request(id)); e == nil {
		t.Fatal("late command started after cancel")
	}
	// Tombstones are bounded, including when a peer sends nonexistent IDs.
	for i := 0; i < 150; i++ {
		a.cancelRelayDiagnostic(t.Context(), fmt.Sprintf("%032x", i))
	}
	if len(a.diagnosticCanceled) > 128 {
		t.Fatal("unbounded cancellation history")
	}
}

func TestExpiredRelayDiagnosticNeverStarts(t *testing.T) {
	f := &cancelDiagnosticCore{entered: make(chan struct{})}
	a := &Agent{cores: map[model.CoreType]*managed{model.CoreXray: {adapter: f}}}
	req, _ := json.Marshal(model.RelayBusinessRequest{ID: strings.Repeat("e", 32), ExpiresAt: time.Now().Add(-time.Second).Unix()})
	if _, err := a.relayBusinessProbe(t.Context(), string(req)); err == nil {
		t.Fatal("expired task accepted")
	}
	select {
	case <-f.entered:
		t.Fatal("expired task installed credentials")
	default:
	}
}
