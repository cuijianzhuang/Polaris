package store

import "time"

// UserSSOBinding ties an external SSO account to a registered user
// (store.User). It is the user-side twin of SSOBinding, deliberately a
// separate table (docs/design/user-identity-oauth-passkey.md D4): the
// admin table's rows name admins, and the two namespaces must never be
// read across — the same (provider, subject) may legitimately be bound
// to an admin and to a user at once, as different accounts. A user may
// bind one account per provider, hence the composite primary key.
type UserSSOBinding struct {
	ProviderID string    `gorm:"primaryKey;size:32" json:"providerId"`
	Subject    string    `gorm:"primaryKey;size:255" json:"-"`
	UserID     string    `gorm:"size:32;index" json:"-"`
	Login      string    `gorm:"size:255" json:"login"`
	CreatedAt  time.Time `json:"createdAt"`
}
