package store

import (
	"github.com/cuijianzhuang/polaris/internal/model"
	"time"
)

type RouteTraceComparison struct {
	State       string `json:"state"` // baseline / same / changed / not_comparable
	PreviousID  int64  `json:"previousId,omitempty"`
	Reason      string `json:"reason,omitempty"`
	ChangedTTLs []int  `json:"changedTtls"`
}

// Runtime observations, excluded from configuration backups. IDs order attempt
// creation, not completion or the remote Agent clock.
type NodeRouteTrace struct {
	ID         int64                        `gorm:"primaryKey;autoIncrement" json:"id"`
	NodeID     string                       `gorm:"size:32;index:idx_route_trace_scope,priority:1" json:"nodeId"`
	Family     string                       `gorm:"size:8;index:idx_route_trace_scope,priority:2" json:"family"`
	Target     string                       `gorm:"size:45;index:idx_route_trace_scope,priority:3" json:"target"`
	RunID      string                       `gorm:"size:32;index" json:"runId"`
	Profile    string                       `gorm:"size:32" json:"profile"`
	State      string                       `gorm:"size:16" json:"state"` // running / completed / failed / interrupted
	StartedAt  time.Time                    `gorm:"index" json:"startedAt"`
	FinishedAt *time.Time                   `json:"finishedAt,omitempty"`
	Result     JSON[model.RouteTraceResult] `json:"result"`
	Comparison JSON[RouteTraceComparison]   `json:"comparison"`
}
