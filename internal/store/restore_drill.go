package store

import "time"

type RestoreDrillCheck struct {
	Name   string `json:"name"`
	State  string `json:"state"` // passed / skipped / failed
	Detail string `json:"detail,omitempty"`
}

// RestoreDrill is an operational record, never restored from a business backup.
type RestoreDrill struct {
	NeedPassphrase          bool                      `gorm:"-" json:"needPassphrase,omitempty"`
	ID                      string                    `gorm:"primaryKey;size:32" json:"id"`
	Source                  string                    `gorm:"size:255" json:"source"`
	Status                  string                    `gorm:"size:16" json:"status"`
	StartedAt               time.Time                 `json:"startedAt"`
	BackupAt                *time.Time                `json:"backupAt,omitempty"`
	FinishedAt              time.Time                 `json:"finishedAt"`
	DurationMS              int64                     `json:"durationMs"`
	RecoveryPointAgeSeconds *int64                    `json:"recoveryPointAgeSeconds,omitempty"`
	SHA256                  string                    `gorm:"size:64" json:"sha256"`
	Checks                  JSON[[]RestoreDrillCheck] `json:"checks"`
	Counts                  JSON[map[string]int64]    `json:"counts"`
	Error                   string                    `gorm:"size:1000" json:"error,omitempty"`
}
