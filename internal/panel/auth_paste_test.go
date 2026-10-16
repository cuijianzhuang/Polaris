package panel

import "testing"

// Credentials pasted from a terminal or typed on a phone keyboard.
func TestLoginToleratesPastedCredentials(t *testing.T) {
	_, srv, c := newAdminClient(t)
	anon := &apiClient{t: t, base: c.base}
	login := func(user, pw string) int {
		st, _ := anon.do("POST", "/auth/login", map[string]string{"username": user, "password": pw})
		return st
	}
	for _, tc := range []struct{ user, pw string }{
		{"root", "old-password\n"}, {" root ", "  old-password "}, {"Root", "old-password"},
	} {
		if st := login(tc.user, tc.pw); st != 200 {
			t.Fatalf("login %q / %q: %d", tc.user, tc.pw, st)
		}
	}
	if st := login("root", "old-passwor"); st != 401 {
		t.Fatalf("wrong password accepted: %d", st)
	}
	if st := login("root", "OLD-PASSWORD"); st != 401 {
		t.Fatalf("password must stay case-sensitive: %d", st)
	}

	if st, out := c.do("POST", "/auth/password", map[string]string{"old": "old-password ", "new": " spaced-new "}); st != 400 || out["error"] != "新密码首尾不能有空格" {
		t.Fatalf("spaced new password: %d %v", st, out)
	}
	st, out := c.do("POST", "/auth/password", map[string]string{"old": "old-password ", "new": "new-password"})
	if st != 200 {
		t.Fatalf("change: %d %v", st, out)
	}
	if login("root", "new-password") != 200 || login("root", "old-password") != 401 {
		t.Fatal("new password must work and the old one must not")
	}
	_ = srv
}
