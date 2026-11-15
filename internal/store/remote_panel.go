package store

import "time"

// RemotePanel is one independent panel instance registered on this hub.
type RemotePanel struct {
	ID        string         `gorm:"primaryKey;size:32" json:"id"`
	Name      string         `gorm:"size:64;uniqueIndex" json:"name"`
	BaseURL   string         `gorm:"size:255" json:"baseUrl"`
	Token     string         `gorm:"type:text" json:"-"` // sealed by Box
	Scopes    JSON[[]string] `json:"scopes"`
	CAPin     string         `gorm:"size:64" json:"caPin"`
	Insecure  bool           `gorm:"default:false" json:"insecure"`
	Labels    JSON[[]string] `json:"labels"`
	Enabled   bool           `gorm:"default:true" json:"enabled"`
	CreatedBy string         `gorm:"size:64" json:"createdBy"`

	// Latest snapshot, for display and backoff; not business authority.
	Snapshot   JSON[RemoteSnapshot] `json:"snapshot"`
	SnapshotAt *time.Time           `json:"snapshotAt"`
	LastOKAt   *time.Time           `json:"lastOkAt"`
	LastError  string               `gorm:"size:255" json:"lastError"` // fixed reason code
	Failures   int                  `gorm:"default:0" json:"failures"`
	NextTryAt  *time.Time           `json:"nextTryAt"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RemoteSnapshot is the cached read-only summary from a member panel.
// Traffic is the current calendar month, so hubs can sum it across members
// without worrying about mismatched accounting windows.
type RemoteSnapshot struct {
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
	Users        int      `json:"users"`
	OnlineUsers  int      `json:"onlineUsers"`
	Nodes        int      `json:"nodes"`
	NodesOnline  int      `json:"nodesOnline"`
	TrafficUp    int64    `json:"trafficUp"`   // calendar month
	TrafficDown  int64    `json:"trafficDown"` // calendar month
}
