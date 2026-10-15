package agent

import (
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Heartbeat inputs and rev requests must not wait for a running apply
// (installing a core can take minutes; the panel would see the node time out).
func TestReadersDoNotWaitForApply(t *testing.T) {
	a := &Agent{cores: map[model.CoreType]*managed{}, wantRev: make(chan int64, 1)}
	a.rev.Store(7)
	a.applyMu.Lock() // an apply in progress
	defer a.applyMu.Unlock()
	done := make(chan struct{})
	go func() {
		_ = a.appliedRev()
		_ = a.coreSnapshot()
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ { // racing producers, nobody consuming
			wg.Add(1)
			go func(r int64) { defer wg.Done(); a.requestRev(r) }(int64(i))
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat path blocked behind the apply lock")
	}
	if got := a.appliedRev(); got != 7 {
		t.Fatalf("appliedRev = %d", got)
	}
}
