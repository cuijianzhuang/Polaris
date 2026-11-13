package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// The last batch has no successor to protect, so waiting out its soak only
// delays "done". It must finish as soon as it is healthy once, without
// faking the soak clock.
func TestRolloutFinalBatchSkipsSoak(t *testing.T) {
	s := newTestServer(t)
	rolloutNodesFixture(t, s, 1)
	(&fakeFleet{}).install(t, s)
	r, err := s.newRollout(rolloutReq{Kind: "agent", Canary: 1, BatchSize: 1, SoakSeconds: 120}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	done := false
	for i := 0; i < 10 && !done; i++ {
		s.runRollouts()
		var cur store.UpgradeRollout
		s.st.DB.First(&cur, "id = ?", r.ID)
		done = cur.State == "done"
	}
	if !done {
		var cur store.UpgradeRollout
		s.st.DB.First(&cur, "id = ?", r.ID)
		t.Fatalf("rollout did not finish without the soak: state %s", cur.State)
	}
}

func TestRolloutDefaultSoakSeconds(t *testing.T) {
	s := newTestServer(t)
	rolloutNodesFixture(t, s, 1)
	(&fakeFleet{}).install(t, s)
	r, err := s.newRollout(rolloutReq{Kind: "agent", Canary: 1, BatchSize: 1}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if r.SoakSeconds != 60 {
		t.Fatalf("default soak %d, want 60", r.SoakSeconds)
	}
}
