package store

import "time"

// UserPasskey stores an encrypted WebAuthn credential for a registered
// user (store.User); the private key never leaves the authenticator.
// It is the user-side twin of Passkey, deliberately a separate table
// (docs/design/user-identity-oauth-passkey.md D1): the admin table is
// referenced by admin account-recovery and backup logic whose blast
// radius must never reach user credentials, so the two tables share a
// shape but never a row, a query or a revocation path.
type UserPasskey struct {
	ID             string     `gorm:"primaryKey;size:32" json:"id"`
	UserID         string     `gorm:"size:32;index" json:"-"`
	CredentialHash string     `gorm:"size:64;uniqueIndex" json:"-"`
	RPID           string     `gorm:"size:255" json:"rpId"`
	Name           string     `gorm:"size:64" json:"name"`
	Data           string     `gorm:"type:text" json:"-"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastUsedAt     *time.Time `json:"lastUsedAt,omitempty"`
}
