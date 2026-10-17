package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func buttonData(rows [][]button) string {
	var b strings.Builder
	for _, r := range rows {
		for _, x := range r {
			b.WriteString(x.Data + " " + x.Text + "\n")
		}
	}
	return b.String()
}

// TestTelegramMenu: the main menu offers each identity only what it may use,
// and every view leads back to the menu.
func TestTelegramMenu(t *testing.T) {
	s := newTestServer(t)
	licenseAll(s)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "tokyo", Status: "online"})
	db.Create(&store.Node{ID: "n2", Name: "hk", Status: "offline"})
	for i := 0; i < 11; i++ {
		db.Create(&store.User{ID: "u" + string(rune('a'+i)), Username: "user" + string(rune('a'+i)), UUID: "uuid-" + string(rune('a'+i)), SubToken: "t" + string(rune('a'+i)), Status: "active", TrafficLimit: 1000, UsedDown: 400})
	}
	owner := tgIdentity{admin: &store.TGAdmin{ID: "o", Name: "boss", Role: "owner", Notify: true}}
	op := tgIdentity{admin: &store.TGAdmin{ID: "p", Name: "op", Role: "operator", NodeIDs: store.NewJSON([]string{"n2"})}}
	var u store.User
	db.First(&u, "id = ?", "ua")
	user := tgIdentity{user: &u}
	b := s.tg

	_, rows := b.view("home", "", owner)
	if m := buttonData(rows); !strings.Contains(m, "v:users:0") || !strings.Contains(m, "m:login") || !strings.Contains(m, "🔔") {
		t.Fatalf("owner menu:\n%s", m)
	}
	_, rows = b.view("home", "", op)
	if m := buttonData(rows); strings.Contains(m, "v:users") || strings.Contains(m, "v:top") || !strings.Contains(m, "n:l") || !strings.Contains(m, "🔕") {
		t.Fatalf("operator menu:\n%s", m)
	}
	text, rows := b.view("home", "", user)
	if m := buttonData(rows); strings.Contains(m, "n:l") || strings.Contains(m, "m:login") || !strings.Contains(m, "v:me") || !strings.Contains(text, "usera") {
		t.Fatalf("user menu:\n%s\n%s", text, m)
	}
	if text, _ := b.view("me", "", user); !strings.Contains(text, "▰▰▰▰▱▱▱▱▱▱") || !strings.Contains(text, "40.0%") {
		t.Fatalf("user card: %s", text)
	}
	// Restricted views refuse quietly.
	if text, _ := b.view("users", "0", op); !strings.Contains(text, "仅超级管理员") {
		t.Fatalf("operator users view: %s", text)
	}
	if text, _ := b.view("status", "", op); strings.Contains(text, "tokyo") || !strings.Contains(text, "hk") {
		t.Fatalf("operator status: %s", text)
	}
	// Users list pages by 8.
	text, rows = b.view("users", "1", owner)
	if m := buttonData(rows); !strings.Contains(text, "（11）") || !strings.Contains(m, "2 / 2") || !strings.Contains(m, "v:users:0 ‹") {
		t.Fatalf("users page 2:\n%s\n%s", text, m)
	}
	for _, v := range []string{"me", "nodes", "status", "users", "top", "help", "bindhelp"} {
		for _, id := range []tgIdentity{owner, op, user, {}} {
			if _, rows := b.view(v, "", id); !strings.Contains(buttonData(rows), "v:home") {
				t.Fatalf("view %s has no way back to the menu", v)
			}
		}
	}
}
