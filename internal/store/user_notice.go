package store

import "time"

// UserNotice is a durable per-channel reminder. Acknowledging its creation
// means scheduled, never delivered. The provider may receive a retry after
// a lost response; neither SMTP nor Telegram promises exactly-once delivery.
type UserNotice struct {
	ID         string    `gorm:"primaryKey;size:64" json:"id"`
	UserID     string    `gorm:"size:32;index" json:"userId"`
	Kind       string    `gorm:"size:32" json:"kind"`
	Channel    string    `gorm:"size:16" json:"channel"`
	Recipient  string    `json:"-"`
	Text       string    `json:"-"`
	Term       string    `gorm:"size:128" json:"-"`
	Tier       int       `json:"tier"`
	Status     string    `gorm:"size:16;index" json:"status"` // pending / sent / canceled
	Attempts   int       `json:"attempts"`
	NextAt     time.Time `gorm:"index" json:"nextAt"`
	LeaseUntil time.Time `json:"-"`
	LeaseToken string    `gorm:"size:32" json:"-"`
	LastError  string    `gorm:"size:200" json:"lastError,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}
