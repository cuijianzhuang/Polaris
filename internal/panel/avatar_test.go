package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func TestTelegramOperatorAvatarIsSelfOnly(t *testing.T) {
	s, _, owner := newAdminClient(t)
	current := store.TGAdmin{ID: "avatar-tg", ChatID: 1234, Name: "avatar-tg", Role: "operator", NodeIDs: store.NewJSON([]string{"avatar-node"})}
	other := store.TGAdmin{ID: "avatar-other", ChatID: 5678, Name: "avatar-other", Role: "owner", AvatarEmail: "other@example.com"}
	for _, value := range []any{&current, &other} {
		if err := s.st.DB.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	link := s.tgLoginURL(s.newTGLoginCode(current.ID))
	_, code, ok := strings.Cut(link, "#tg=")
	if !ok {
		t.Fatal("missing login code")
	}
	client := &apiClient{t: t, base: owner.base}
	status, out := client.do("POST", "/auth/tg", map[string]any{"code": code})
	if status != 200 {
		t.Fatal(status, out)
	}
	client.tok = out["token"].(string)
	status, out = client.do("PUT", "/auth/avatar", map[string]any{"email": "alice@example.com", "id": other.ID})
	if status != 200 || out["avatarURL"] != sub.AvatarURL("alice@example.com") {
		t.Fatal(status, out)
	}
	var saved store.TGAdmin
	s.st.DB.First(&saved, "id = ?", other.ID)
	if saved.AvatarEmail != "other@example.com" {
		t.Fatal("another account's avatar changed")
	}
	_, me := client.do("GET", "/auth/me", nil)
	if me["avatarURL"] != out["avatarURL"] {
		t.Fatal("Telegram avatar not returned")
	}
}

func TestAvatarPreferenceIsSelfOnly(t *testing.T) {
	s, _, owner := newAdminClient(t)
	if err := s.st.DB.Create(&store.Node{ID: "avatar-node", Name: "avatar-node"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"owner", "operator", "viewer", "support", "finance"} {
		var client *apiClient
		var id string
		if role == "owner" {
			client, id = owner, "a1"
		} else {
			var ids []string
			if role == "operator" || role == "viewer" {
				ids = []string{"avatar-node"}
			}
			client, id = staffLogin(t, owner, "avatar-"+role, role, ids)
		}
		code, out := client.do("PUT", "/auth/avatar", map[string]any{"email": " Alice@Example.COM ", "id": "someone-else", "role": "owner"})
		if code != 200 || out["avatarEmail"] != "alice@example.com" || out["avatarURL"] != sub.AvatarURL("alice@example.com") {
			t.Fatal(role, code, out)
		}
		var admin store.Admin
		if err := s.st.DB.First(&admin, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		if admin.AvatarEmail != "alice@example.com" || normalizedAdminRole(admin.Role) != role {
			t.Fatal("avatar update changed access", admin.Role)
		}
		_, me := client.do("GET", "/auth/me", nil)
		if me["avatarURL"] != out["avatarURL"] {
			t.Fatal("profile and avatar preference diverged")
		}
		if code, _ := client.do("PUT", "/auth/avatar", map[string]any{"email": "invalid"}); code != 400 {
			t.Fatal("invalid email accepted")
		}
		if code, out := client.do("PUT", "/auth/avatar", map[string]any{"email": ""}); code != 200 || out["avatarURL"] != "" {
			t.Fatal("avatar could not be cleared", code, out)
		}
	}
}

func TestUserAvatarFollowsEmail(t *testing.T) {
	s, _, client := newAdminClient(t)
	u := newUser(&userReq{Username: "avatar-user"})
	u.Email = "alice@example.com"
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	_, response := client.do("GET", "/users/"+u.ID, nil)
	if response["avatarURL"] != sub.AvatarURL(u.Email) {
		t.Fatal("missing user avatar")
	}
	if err := s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("email", "").Error; err != nil {
		t.Fatal(err)
	}
	_, response = client.do("GET", "/users/"+u.ID, nil)
	if value, ok := response["avatarURL"]; ok && value != "" {
		t.Fatal("stale avatar after removing email")
	}
}
