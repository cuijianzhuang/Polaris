package store

import (
	"time"

	"github.com/cuijianzhuang/polaris/internal/sub"
)

// SubViewGroup is a presentation group ("订阅视图组"): it selects a subset of
// a user's already-authorised nodes with a declarative predicate, runs a
// processing chain over them (referenced reusable processing groups followed
// by local operators) and carries presentation attributes. It never grants
// access — it can only subtract from the authorised set; nodes matched by no
// view group fall through to the default group unchanged.
//
// Provisional licensing note: view groups live in the community build with
// no feature gate, matching where user-subscription operator execution
// already lives. If the call is made to move them behind the pro gate, the
// gate belongs in the panel handlers (sub_viewgroups.go) and in
// applyPlanViewGroups — the model itself stays put.
//
// Batch 1 populates Selector / ProcessingGroups / Operators / Kinds /
// ChainIDs; Grouped drives client-side grouping in batch 2.
type SubViewGroup struct {
	ID               string                   `gorm:"primaryKey;size:32" json:"id"`
	Name             string                   `gorm:"size:64;uniqueIndex" json:"name"`
	Remark           string                   `gorm:"size:255" json:"remark"`
	Selector         JSON[sub.Operator]       `json:"selector"` // a single "filter" operator used as the predicate
	ProcessingGroups JSON[[]SubProcessingRef] `json:"processingGroups"`
	Operators        JSON[[]sub.Operator]     `json:"operators"`
	Kinds            JSON[[]string]           `json:"kinds"`    // endpoint kinds allowed in: direct / cdn / relay; empty = all
	ChainIDs         JSON[[]string]           `json:"chainIds"` // relay chain whitelist; empty = all
	Icon             string                   `gorm:"size:32" json:"icon"`
	SortOrder        int                      `json:"sortOrder"`
	Pinned           bool                     `json:"pinned"`
	Grouped          bool                     `json:"grouped"` // present as its own client group (batch 2)
	Enabled          bool                     `json:"enabled"`
	Revision         int64                    `json:"revision"`
	CreatedAt        time.Time                `json:"createdAt"`
	UpdatedAt        time.Time                `json:"updatedAt"`
}
