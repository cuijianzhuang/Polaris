package mihomo

import (
	"context"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Read more frequently than the agent's durable batch interval. The upstream
// API still omits closed connections: this is sampling, never exact billing.
const meterInterval = time.Second

type trafficMeter struct {
	sampleMu  sync.Mutex
	mu        sync.Mutex
	pending   map[string][2]int64
	status    model.TrafficMeter
	lifecycle sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
}

func (a *Adapter) startMeter() {
	a.meter.lifecycle.Lock()
	defer a.meter.lifecycle.Unlock()
	if a.meter.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.meter.cancel, a.meter.done = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(meterInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = a.sampleTraffic(ctx)
			}
		}
	}()
}

func (a *Adapter) stopMeter() {
	a.meter.lifecycle.Lock()
	defer a.meter.lifecycle.Unlock()
	if a.meter.cancel != nil {
		a.meter.cancel()
		<-a.meter.done
		a.meter.cancel = nil
	}
}

func (a *Adapter) TrafficMeterStatus() model.TrafficMeter {
	a.meter.mu.Lock()
	defer a.meter.mu.Unlock()
	out := a.meter.status
	out.Mode = "connection_sampled"
	out.IntervalSeconds = int(meterInterval / time.Second)
	return out
}

func (a *Adapter) sampleTraffic(ctx context.Context) error {
	a.meter.sampleMu.Lock()
	defer a.meter.sampleMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conns, err := a.connections(ctx)
	now := time.Now().UTC()
	a.meter.mu.Lock()
	defer a.meter.mu.Unlock()
	a.meter.status.LastAttempt = &now
	if err != nil {
		a.meter.status.ConsecutiveFailures++
		return err
	}
	a.meter.status.LastSuccess = &now
	a.meter.status.ConsecutiveFailures = 0
	if a.meter.pending == nil {
		a.meter.pending = map[string][2]int64{}
	}
	// Ignore malformed/repeated IDs. A counter decrease starts a new baseline;
	// it must not subtract another user's usage or invent a billable increment.
	seen := map[string][2]int64{}
	for _, c := range conns {
		if c.ID == "" || c.Upload < 0 || c.Download < 0 {
			continue
		}
		if _, duplicate := seen[c.ID]; duplicate {
			continue
		}
		seen[c.ID] = [2]int64{c.Upload, c.Download}
		prev := a.conns[c.ID]
		if c.Metadata.InboundUser == "" {
			continue
		}
		v := a.meter.pending[c.Metadata.InboundUser]
		v[0] += max(0, c.Upload-prev[0])
		v[1] += max(0, c.Download-prev[1])
		a.meter.pending[c.Metadata.InboundUser] = v
	}
	a.conns = seen
	return nil
}
