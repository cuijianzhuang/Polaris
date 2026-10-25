package store

import "time"

// Passkey stores an encrypted WebAuthn credential; the private key never leaves
// the authenticator. Only its hash is exposed to the database lookup index.
type Passkey struct {
	ID             string     `gorm:"primaryKey;size:32" json:"id"`
	AdminID        string     `gorm:"size:32;index" json:"-"`
	CredentialHash string     `gorm:"size:64;uniqueIndex" json:"-"`
	RPID           string     `gorm:"size:255" json:"rpId"`
	Name           string     `gorm:"size:64" json:"name"`
	Data           string     `gorm:"type:text" json:"-"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastUsedAt     *time.Time `json:"lastUsedAt,omitempty"`
}

// AccountRecoveryCode is a hash of a 192-bit offline recovery secret. These
// codes are separate from TOTP recovery codes and authorize a full reset.
type AccountRecoveryCode struct {
	Hash      string    `gorm:"primaryKey;size:64" json:"-"`
	AdminID   string    `gorm:"size:32;index" json:"-"`
	CreatedAt time.Time `json:"createdAt"`
}
