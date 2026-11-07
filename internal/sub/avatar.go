package sub

import (
	"crypto/sha256"
	"fmt"

	"github.com/cuijianzhuang/polaris/internal/mailer"
)

// AvatarURL uses Gravatar's SHA-256 email identifier. Never include an email
// address or a subscription credential in the remote URL.
func AvatarURL(email string) string {
	email = mailer.Normalize(email)
	if email == "" {
		return ""
	}
	return fmt.Sprintf("https://www.gravatar.com/avatar/%x?s=128&d=identicon&r=g", sha256.Sum256([]byte(email)))
}
