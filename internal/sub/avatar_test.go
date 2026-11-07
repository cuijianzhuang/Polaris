package sub

import (
	"net/url"
	"strings"
	"testing"
)

func TestAvatarURL(t *testing.T) {
	avatar := AvatarURL(" Alice@Example.com ")
	parsed, err := url.Parse(avatar)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "www.gravatar.com" {
		t.Fatal("unsafe avatar URL", avatar)
	}
	if parsed.Path != "/avatar/ff8d9819fc0e12bf0d24892e45987e249a28dce836a85cad60e28eaaa8c6d976" {
		t.Fatal("incorrect normalized SHA-256 identifier", parsed.Path)
	}
	if strings.Contains(avatar, "alice") || strings.Contains(avatar, "example.com") || parsed.Query().Get("d") != "identicon" {
		t.Fatal("email leak or missing fallback", avatar)
	}
	for _, email := range []string{"", "bad", "alice@example.com\r\nX: evil"} {
		if AvatarURL(email) != "" {
			t.Fatal("invalid email used for remote avatar")
		}
	}
}
