package store

import (
	"github.com/cuijianzhuang/polaris/internal/model"
	"time"
)

// Configuration is backed up, execution cursors are not.
type NodeRouteSchedule struct {
	NodeID          string                        `gorm:"primaryKey;size:32" json:"nodeId"`
	Enabled         bool                          `json:"enabled"`
	Targets         JSON[model.RouteTraceRequest] `json:"targets"`
	IntervalMinutes int                           `json:"intervalMinutes"`
	Revision        int64                         `json:"revision"`
	UpdatedAt       time.Time                     `json:"updatedAt"`
}

type NodeRouteScheduleState struct {
	NodeID        string     `gorm:"primaryKey;size:32" json:"nodeId"`
	Revision      int64      `json:"revision"`
	NextAt        time.Time  `gorm:"index" json:"nextAt"`
	LastStartedAt *time.Time `json:"lastStartedAt,omitempty"`
	LastRunID     string     `gorm:"size:32" json:"lastRunId,omitempty"`
	LastState     string     `gorm:"size:24" json:"lastState"`
}
