package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func (a *Agent) relayBusinessProbe(ctx context.Context, raw string) (model.RelayBusinessResult, error) {
	unsupported := model.RelayBusinessResult{State: "unsupported", Checks: []model.BusinessCheck{}}
	var req model.RelayBusinessRequest
	if len(raw) > 8192 || json.Unmarshal([]byte(raw), &req) != nil || len(req.ID) != 32 {
		return unsupported, fmt.Errorf("invalid diagnostic request")
	}
	expiry := time.Unix(req.ExpiresAt, 0)
	if !expiry.After(time.Now()) || expiry.After(time.Now().Add(30*time.Second)) {
		return unsupported, fmt.Errorf("diagnostic expired or clock skew too large")
	}
	ctx, cancel := context.WithDeadline(ctx, expiry)
	defer cancel()
	a.diagnosticMu.Lock()
	if a.diagnosticID != "" || a.diagnosticCanceled[req.ID].After(time.Now()) {
		a.diagnosticMu.Unlock()
		return unsupported, fmt.Errorf("diagnostic busy or canceled")
	}
	done := make(chan struct{})
	a.diagnosticID, a.diagnosticCancel, a.diagnosticDone = req.ID, cancel, done
	a.diagnosticMu.Unlock()
	defer func() {
		a.diagnosticMu.Lock()
		a.diagnosticID, a.diagnosticCancel, a.diagnosticDone = "", nil, nil
		close(done)
		a.diagnosticMu.Unlock()
	}()
	// A test does not queue behind installs, mutate a changing core or pause its
	// traffic collection for an unbounded time. Concurrent tests return busy.
	if !a.applyMu.TryLock() {
		return unsupported, fmt.Errorf("node is applying configuration or running another diagnostic")
	}
	defer a.applyMu.Unlock()
	m := a.cores[model.CoreXray]
	if m == nil {
		return unsupported, fmt.Errorf("Xray is not running on this node")
	}
	runner, ok := m.adapter.(interface {
		RunRelayDiagnostic(context.Context, model.RelayBusinessRequest) (model.RelayBusinessResult, error)
	})
	if !ok {
		return unsupported, fmt.Errorf("core does not support isolated business diagnostics")
	}
	return runner.RunRelayDiagnostic(ctx, req)
}

func (a *Agent) cancelRelayDiagnostic(ctx context.Context, id string) error {
	if len(id) != 32 {
		return fmt.Errorf("invalid diagnostic id")
	}
	a.diagnosticMu.Lock()
	var done chan struct{}
	if a.diagnosticID == id {
		a.diagnosticCancel()
		done = a.diagnosticDone
	} else {
		if a.diagnosticCanceled == nil {
			a.diagnosticCanceled = map[string]time.Time{}
		}
		for key, expiry := range a.diagnosticCanceled {
			if !expiry.After(time.Now()) {
				delete(a.diagnosticCanceled, key)
			}
		}
		if len(a.diagnosticCanceled) >= 128 {
			a.diagnosticMu.Unlock()
			return fmt.Errorf("too many canceled diagnostics")
		}
		a.diagnosticCanceled[id] = time.Now().Add(30 * time.Second)
	}
	a.diagnosticMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(4 * time.Second):
			return fmt.Errorf("diagnostic cleanup still in progress")
		}
	}
	return nil
}
