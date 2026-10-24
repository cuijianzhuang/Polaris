package store

import "time"

// PaymentReceipt is written before acknowledging a trusted payment callback.
// A delivery transaction can be retried independently without charging twice.
type PaymentReceipt struct {
	Ref        string    `gorm:"primaryKey;size:128" json:"ref"`
	OrderID    string    `gorm:"size:32;index" json:"orderId"`
	Method     string    `gorm:"size:16" json:"method"`
	Amount     string    `gorm:"size:32" json:"amount"`
	Currency   string    `gorm:"size:8" json:"currency"`
	Status     string    `gorm:"size:16;index" json:"status"` // pending / done / review
	Attempts   int       `json:"attempts"`
	NextAt     time.Time `gorm:"index" json:"nextAt"`
	LeaseUntil time.Time `json:"-"`
	LeaseToken string    `gorm:"size:32" json:"-"`
	LastError  string    `gorm:"size:255" json:"lastError"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// BuyerLock serializes creation of the first account for a Telegram buyer.
// Existing accounts also take the same user-row lock as admin/API commands.
type ShopBuyerLock struct {
	Key       string `gorm:"primaryKey;size:128" json:"-"`
	UpdatedAt time.Time
}

// RefundJob freezes the original bot credentials; secrets never enter APIs.
// requesting/confirming are uncertain results, not a fabricated refund.
type RefundJob struct {
	OrderID    string    `gorm:"primaryKey;size:32" json:"orderId"`
	Ref        string    `gorm:"size:128" json:"-"`
	Buyer      int64     `json:"-"`
	BotToken   string    `gorm:"type:text" json:"-"`
	APIBase    string    `gorm:"size:1024" json:"-"`
	Status     string    `gorm:"size:16;index" json:"status"` // pending / requesting / confirming / confirmed / done
	Attempts   int       `json:"attempts"`
	NextAt     time.Time `gorm:"index" json:"nextAt"`
	LeaseUntil time.Time `json:"-"`
	LeaseToken string    `gorm:"size:32" json:"-"`
	LastError  string    `gorm:"size:255" json:"lastError"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Cluster operations outlive transient LISTEN/NOTIFY connections.
// accepted means the local request was written / restart was scheduled;
// updater results remain a separate, authoritative success/failure report.
type ClusterOperation struct {
	ID        string    `gorm:"primaryKey;size:32" json:"id"`
	Kind      string    `gorm:"size:16" json:"kind"`
	Target    string    `gorm:"size:128" json:"target"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `gorm:"index" json:"expiresAt"`
}
type ClusterDelivery struct {
	OperationID   string     `gorm:"primaryKey;size:32" json:"operationId"`
	MemberID      string     `gorm:"primaryKey;size:32" json:"memberId"`
	BeforeVersion string     `gorm:"size:128" json:"-"`
	Status        string     `gorm:"size:16;index" json:"status"` // pending / accepted
	Attempts      int        `json:"attempts"`
	NextAt        time.Time  `json:"nextAt"`
	LastError     string     `gorm:"size:255" json:"lastError"`
	AcceptedAt    *time.Time `json:"acceptedAt,omitempty"`
}

type OperationalStatus struct {
	Key         string     `gorm:"primaryKey;size:128" json:"key"`
	Detail      string     `gorm:"size:255" json:"detail"`
	Failed      bool       `json:"failed"`
	FirstSeen   time.Time  `json:"firstSeen"`
	LastSuccess *time.Time `json:"lastSuccess,omitempty"`
	LastFailure *time.Time `json:"lastFailure,omitempty"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// RelayBusinessStatus retains the latest standard-target diagnostic attempt.
// It contains no temporary identity, target address, or raw Agent message.
type RelayBusinessStatus struct {
	ChainID          string `gorm:"primaryKey;size:32"`
	InboundID        string `gorm:"size:32;index"`
	NodeID           string `gorm:"size:32;index"`
	RunID            string `gorm:"size:32"`
	Fingerprint      string `gorm:"size:64"`
	State            string `gorm:"size:16"`
	StartedAt        time.Time
	CheckedAt        *time.Time
	BusinessVerified bool
	CleanupVerified  bool
	FailedChecks     JSON[[]string]
}
