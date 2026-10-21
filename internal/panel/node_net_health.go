package panel

import (
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
)

const nodeNetStaleAfter = 2 * time.Minute

type nodeNetHealth struct {
	State             string     `json:"state"` // never, healthy, stale; describes accepted samples, not node connectivity
	Mode              string     `json:"mode"`  // boot, epoch, legacy, unknown
	CheckedAt         time.Time  `json:"checkedAt"`
	LastSampleAt      *time.Time `json:"lastSampleAt,omitempty"`
	SampleAgeSeconds  *int64     `json:"sampleAgeSeconds,omitempty"`
	StaleAfterSeconds int64      `json:"staleAfterSeconds"`
	BaselineAt        *time.Time `json:"baselineAt,omitempty"`
	BaselineReason    string     `json:"baselineReason,omitempty"`
	LastGapAt         *time.Time `json:"lastGapAt,omitempty"`
	LastGapSeconds    int64      `json:"lastGapSeconds"`
}

func netCollectionMode(epoch, boot string) string {
	if boot != "" {
		return "boot"
	}
	if epoch != "" {
		return "epoch"
	}
	return "legacy"
}

func nodeNetHealthOf(n *store.Node, now time.Time) nodeNetHealth {
	h := nodeNetHealth{State: "never", Mode: "unknown", CheckedAt: now, StaleAfterSeconds: int64(nodeNetStaleAfter.Seconds()),
		BaselineAt: n.NetBaselineAt, BaselineReason: n.NetBaselineReason, LastGapAt: n.NetLastGapAt, LastGapSeconds: n.NetLastGapSeconds}
	if n.NetObservedAt == nil || n.NetObservedAt.IsZero() {
		return h
	}
	h.LastSampleAt = n.NetObservedAt
	h.Mode = netCollectionMode(n.NetEpoch, n.NetBootID)
	age := max(int64(0), int64(now.Sub(*n.NetObservedAt).Seconds()))
	h.SampleAgeSeconds = &age
	h.State = "healthy"
	if age >= h.StaleAfterSeconds {
		h.State = "stale"
	}
	return h
}

// Reasons describe the evidence available in the sample, without guessing
// reboot vs interface replacement when an older agent lacks a separate boot ID.
func nodeNetRebaseReason(n *store.Node, sample *polarisv1.SystemStats, epoch string) string {
	if !n.NetBaseline && n.NetLastRx == 0 && n.NetLastTx == 0 {
		return "initial"
	}
	if netCollectionMode(n.NetEpoch, n.NetBootID) != netCollectionMode(epoch, sample.NetBootId) {
		return "format_changed"
	}
	if n.NetBootID != sample.NetBootId {
		return "boot_changed"
	}
	if n.NetEpoch != epoch {
		if n.NetBootID != "" {
			return "interfaces_changed"
		}
		return "source_changed"
	}
	if sample.RxBytes < n.NetLastRx || sample.TxBytes < n.NetLastTx {
		return "counters_decreased"
	}
	return ""
}
