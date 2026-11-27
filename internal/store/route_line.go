package store

import "time"

// NodeRouteLine is the heuristic line classification of one finished route
// trace (design docs/design/backhaul-monitor.md D7). One row per trace,
// written when the trace completes; only an evidence summary is kept, the
// hop-by-hop path stays in NodeRouteTrace. Runtime observation data like
// NodeRouteTrace itself: excluded from configuration backups, and rows are
// deleted together with the trace they classify.
type NodeRouteLine struct {
	TraceID           int64     `gorm:"primaryKey" json:"traceId"`
	NodeID            string    `gorm:"size:32;index:idx_route_line_scope,priority:1" json:"nodeId"`
	Family            string    `gorm:"size:8;index:idx_route_line_scope,priority:2" json:"family"`
	Target            string    `gorm:"size:45;index:idx_route_line_scope,priority:3" json:"target"`
	Carrier           string    `gorm:"size:8" json:"carrier"`     // ct / cu / cm / "" (line's operator; empty when unknown)
	Line              string    `gorm:"size:16" json:"line"`       // cn2 / 163 / 9929 / 4837 / cmin2 / cmi / cmnet / unknown / unmeasured
	Confidence        string    `gorm:"size:16" json:"confidence"` // segment / asn / suspect / none
	Evidence          string    `gorm:"size:1024" json:"evidence"` // JSON summary of matched features, never the full path
	ClassifierVersion int       `json:"classifierVersion"`
	CreatedAt         time.Time `json:"createdAt"`
}
