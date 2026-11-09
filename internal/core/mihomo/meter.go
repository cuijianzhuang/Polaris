package mihomo

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Read more frequently than the agent's durable batch interval. The upstream
// API omits closed connections unless the Polaris cumulative-counter extension
// is present. Stock builds remain explicitly marked as sampled.
const meterInterval = time.Second

type trafficMeter struct {
	sampleMu     sync.Mutex
	mu           sync.Mutex
	pending      map[string][2]int64
	status       model.TrafficMeter
	nativeEpoch  string
	nativeTotals map[string]userTotals
	lifecycle    sync.Mutex
	cancel       context.CancelFunc
	done         chan struct{}
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
	if a.meter.nativeEpoch != "" {
		out.Mode = "kernel_counter"
	} else {
		out.Mode = "connection_sampled"
	}
	out.IntervalSeconds = int(meterInterval / time.Second)
	return out
}

func (a *Adapter) sampleTraffic(ctx context.Context) error {
	a.meter.sampleMu.Lock()
	defer a.meter.sampleMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	snapshot, err := a.connectionSnapshot(ctx)
	now := time.Now().UTC()
	a.meter.mu.Lock()
	defer a.meter.mu.Unlock()
	a.meter.status.LastAttempt = &now
	if err != nil {
		a.meter.status.ConsecutiveFailures++
		return err
	}
	if err := a.recordNativeTraffic(snapshot); err != nil {
		a.meter.status.ConsecutiveFailures++
		return err
	}
	a.meter.status.LastSuccess = &now
	a.meter.status.ConsecutiveFailures = 0
	if a.meter.pending == nil {
		a.meter.pending = map[string][2]int64{}
	}
	if snapshot.UserTraffic != nil {
		return nil
	}
	// Ignore malformed/repeated IDs. A counter decrease starts a new baseline;
	// it must not subtract another user's usage or invent a billable increment.
	seen := map[string][2]int64{}
	for _, c := range snapshot.Connections {
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
		userID := a.userIdentity(c.Metadata.InboundUser)
		v := a.meter.pending[userID]
		v[0] += max(0, c.Upload-prev[0])
		v[1] += max(0, c.Download-prev[1])
		a.meter.pending[userID] = v
	}
	a.conns = seen
	return nil
}

// Called with meter.mu held. Never fall back to connection sampling after
// accepting lifetime counters: that would rebill active connections. A core
// restart has a new epoch; a controller reset preserves the existing epoch.
func (a *Adapter) recordNativeTraffic(snapshot connectionSnapshot) error {
	native := snapshot.UserTraffic
	if native == nil {
		if a.meter.nativeEpoch != "" {
			return fmt.Errorf("mihomo cumulative counters disappeared; refusing sampled fallback")
		}
		return nil
	}
	epoch, err := hex.DecodeString(native.Epoch)
	if err != nil || len(epoch) != 16 || native.Version != 1 || native.Users == nil {
		return fmt.Errorf("invalid mihomo cumulative counter snapshot")
	}
	sameEpoch := native.Epoch == a.meter.nativeEpoch
	for id, value := range native.Users {
		previous := a.meter.nativeTotals[id]
		if id == "" || value.Up < 0 || value.Down < 0 || (sameEpoch && (value.Up < previous.Up || value.Down < previous.Down)) {
			return fmt.Errorf("invalid or decreasing mihomo user counter")
		}
	}
	if sameEpoch {
		for id := range a.meter.nativeTotals {
			if _, ok := native.Users[id]; !ok {
				return fmt.Errorf("mihomo cumulative counter omitted a previously counted user")
			}
		}
	}
	if a.meter.pending == nil {
		a.meter.pending = map[string][2]int64{}
	}
	for id, value := range native.Users {
		previous := userTotals{}
		if sameEpoch {
			previous = a.meter.nativeTotals[id]
		}
		userID := a.userIdentity(id)
		pending := a.meter.pending[userID]
		pending[0] += value.Up - previous.Up
		pending[1] += value.Down - previous.Down
		a.meter.pending[userID] = pending
	}
	a.meter.nativeEpoch, a.meter.nativeTotals = native.Epoch, native.Users
	return nil
}
