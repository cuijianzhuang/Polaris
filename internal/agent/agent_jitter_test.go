package agent

import (
	"testing"
	"time"
)

func TestJitterStaysWithinHalfToOneAndHalf(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 200 {
		d := jitter(10 * time.Second)
		if d < 5*time.Second || d >= 15*time.Second {
			t.Fatalf("jitter out of range: %v", d)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Fatal("jitter is not random")
	}
	if jitter(0) != 0 {
		t.Fatal("zero backoff")
	}
}
