package store

import "time"

// Website stores the public bundle in the logical backup with its routing.
// Certificate material remains in the existing encrypted certificate store.
type Website struct {
	ID            string                  `gorm:"primaryKey;size:32" json:"id"`
	NodeID        string                  `gorm:"size:32;index" json:"nodeId"`
	Domain        string                  `gorm:"size:255" json:"domain"`
	Port          int                     `json:"port"`
	CertificateID string                  `gorm:"size:32;index" json:"certificateId"`
	Enabled       bool                    `json:"enabled"`
	Path          string                  `gorm:"size:255" json:"path"`
	Mode          string                  `gorm:"size:16" json:"mode"`
	Upstream      string                  `gorm:"size:255" json:"upstream"`
	PanelUpstream string                  `gorm:"size:255" json:"panelUpstream"`
	AdminPath     string                  `gorm:"size:255" json:"adminPath"`
	SubPath       string                  `gorm:"size:255" json:"subPath"`
	PortalPath    string                  `gorm:"size:255" json:"portalPath"`
	Files         JSON[map[string][]byte] `json:"files,omitempty"`
	Version       int64                   `json:"version"`
	CreatedAt     time.Time               `json:"createdAt"`
	UpdatedAt     time.Time               `json:"updatedAt"`
}
