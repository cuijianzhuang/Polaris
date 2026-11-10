package panel

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func bindingPlaceholder(t *testing.T, s *Server, name, email string, chat int64) store.User {
	t.Helper()
	past := time.Now().Add(-time.Hour)
	u := newUser(&userReq{Username: name, ExpireAt: &past})
	u.Email, u.TelegramID, u.SelfService = email, chat, true
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u
}
func bindingSubscriber(t *testing.T, s *Server, name string) store.User {
	t.Helper()
	future := time.Now().Add(30 * 24 * time.Hour)
	u := newUser(&userReq{Username: name, TrafficLimit: 1 << 30, ExpireAt: &future})
	u.PlanID = "paid-plan"
	u.UsedUp, u.UsedDown = 10, 20
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u
}
func bindingBrowser(t *testing.T, s *Server, base string, u store.User, mail bool) *portalClient {
	t.Helper()
	token, err := s.issuePortalTokenVia(&u, mail)
	if err != nil {
		t.Fatal(err)
	}
	b := newShopClient(t, base)
	b.setCookie(portalCookie, token)
	return b
}
func bindingPreview(t *testing.T, b *portalClient, token string) map[string]any {
	t.Helper()
	return b.jsonW("POST", "/u/subscription/preview", map[string]any{"subscription": "https://sub.example/sub/" + token + "?format=clash"})
}
func assertBindingUnchanged(t *testing.T, s *Server, before store.User) {
	t.Helper()
	var after store.User
	if err := s.st.DB.First(&after, "id = ?", before.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.SubToken != before.SubToken || after.UUID != before.UUID || after.Password != before.Password || after.SSSeed != before.SSSeed || after.UsedUp != before.UsedUp || after.UsedDown != before.UsedDown || after.TrafficLimit != before.TrafficLimit || after.PlanID != before.PlanID || !after.ExpireAt.Equal(*before.ExpireAt) {
		t.Fatal("binding changed service credentials or entitlements")
	}
}

func TestExistingAccountSubscriptionAssociation(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("portal_email_register", "false")
	registered := bindingPlaceholder(t, s, "registered", "existing@example.com", 0)
	subscriber := bindingSubscriber(t, s, "subscriber")
	subscriber.TelegramID = 4242
	s.st.DB.Model(&subscriber).Update("telegram_id", 4242)
	browser := bindingBrowser(t, s, srv.URL, registered, true)
	originalSubscriber := bindingBrowser(t, s, srv.URL, subscriber, false)
	oldBrowser := bindingBrowser(t, s, srv.URL, registered, true)
	preview := bindingPreview(t, browser, subscriber.SubToken)
	if preview["sameAccount"] != false || preview["username"] != subscriber.Username {
		t.Fatal(preview)
	}
	if st, body := browser.req("POST", "/u/subscription/bind", map[string]any{"ticket": preview["ticket"]}, true); st != 200 {
		t.Fatal(st, body)
	}
	var kept, retired store.User
	s.st.DB.First(&kept, "id = ?", subscriber.ID)
	s.st.DB.First(&retired, "id = ?", registered.ID)
	if kept.Email != registered.Email || kept.TelegramID != 4242 || retired.Status != "disabled" || retired.Email != "" || retired.TelegramID != 0 {
		t.Fatal("identities not associated safely", kept.ID, retired.Status)
	}
	assertBindingUnchanged(t, s, subscriber)
	if st, body := browser.req("GET", "/u", nil, false); st != 200 || !strings.Contains(body, subscriber.SubToken) || !strings.Contains(body, `id="p-account-binding"`) {
		t.Fatal("new session did not use subscription owner", st)
	}
	if st, _ := oldBrowser.req("GET", "/u/sessions", nil, false); st != 401 {
		t.Fatal("retired login survived", st)
	}
	if st, _ := originalSubscriber.req("GET", "/u/sessions", nil, false); st != 200 {
		t.Fatal("original subscription owner session was revoked", st)
	}
	if st, _ := browser.req("POST", "/u/subscription/bind", map[string]any{"ticket": preview["ticket"]}, true); st != 400 {
		t.Fatal("ticket replay", st)
	}
	var count int64
	s.st.DB.Model(&store.User{}).Count(&count)
	if count != 2 {
		t.Fatal("association created or deleted accounts", count)
	}
}

func TestSubscriptionBindingConflictsAndPermissions(t *testing.T) {
	for _, scenario := range []string{"both-provisioned", "order", "legacy-tg-order", "grant", "traffic", "different-email", "different-telegram", "disabled", "reverse-import", "service"} {
		t.Run(scenario, func(t *testing.T) {
			s, srv, _ := newAdminClient(t)
			s.st.SetSetting("portal_enabled", "true")
			current := bindingPlaceholder(t, s, "current", "current@example.com", 0)
			target := bindingSubscriber(t, s, "target")
			switch scenario {
			case "both-provisioned":
				s.st.DB.Model(&current).Updates(map[string]any{"plan_id": "another", "status": "active"})
			case "order":
				s.st.DB.Create(&store.Order{ID: "existing-order", UserID: current.ID, Status: "pending"})
			case "legacy-tg-order":
				current.TelegramID = 100
				s.st.DB.Model(&current).Update("telegram_id", 100)
				s.st.DB.Create(&store.Order{ID: "legacy-order", TelegramID: 100, Status: "pending"})
			case "grant":
				s.st.DB.Create(&store.UserAccess{UserID: current.ID, Kind: "node", TargetID: "node"})
			case "traffic":
				s.st.DB.Create(&store.TrafficDaily{UserID: current.ID, Day: "2026-11-10", Up: 1})
			case "different-email":
				s.st.DB.Model(&target).Update("email", "other@example.com")
			case "different-telegram":
				current.TelegramID = 100
				s.st.DB.Model(&current).Update("telegram_id", 100)
				s.st.DB.Model(&target).Update("telegram_id", 200)
			case "disabled":
				s.st.DB.Model(&target).Update("status", "disabled")
			case "reverse-import":
				current = target
				current.TelegramID = 100
				s.st.DB.Model(&current).Update("telegram_id", 100)
				target = bindingPlaceholder(t, s, "untrusted", "untrusted@example.com", 0)
			case "service":
				s.st.DB.Model(&target).Update("is_service", true)
			}
			browser := bindingBrowser(t, s, srv.URL, current, scenario != "reverse-import")
			if st, body := browser.req("POST", "/u/subscription/preview", map[string]any{"subscription": target.SubToken}, true); st != 409 {
				t.Fatal(scenario, st, body)
			}
			var got store.User
			s.st.DB.First(&got, "id = ?", target.ID)
			if got.Email == "current@example.com" {
				t.Fatal("conflict changed ownership")
			}
		})
	}
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	current := bindingPlaceholder(t, s, "c", "c@example.com", 0)
	target := bindingSubscriber(t, s, "t")
	browser := bindingBrowser(t, s, srv.URL, current, true)
	for _, raw := range []string{"https://sub.example/sub/p/" + target.SubToken, "javascript:alert(1)", "https://other/sub/" + target.SubToken + "/extra"} {
		if st, _ := browser.req("POST", "/u/subscription/preview", map[string]any{"subscription": raw}, true); st != 409 {
			t.Fatal("non-personal subscription accepted", st)
		}
	}
	if st, _ := browser.req("POST", "/u/subscription/preview", map[string]any{"subscription": target.SubToken}, false); st != 403 {
		t.Fatal("no CSRF gate", st)
	}
	if st, _ := newShopClient(t, srv.URL).req("POST", "/u/subscription/preview", map[string]any{"subscription": target.SubToken}, true); st != 401 {
		t.Fatal("unauthenticated association", st)
	}
}

func TestSubscriptionBindingRevalidatesConfirmation(t *testing.T) {
	for _, change := range []string{"target-token", "new-order", "other-session"} {
		t.Run(change, func(t *testing.T) {
			s, srv, _ := newAdminClient(t)
			s.st.SetSetting("portal_enabled", "true")
			current := bindingPlaceholder(t, s, "c", "c@example.com", 0)
			target := bindingSubscriber(t, s, "t")
			browser := bindingBrowser(t, s, srv.URL, current, true)
			preview := bindingPreview(t, browser, target.SubToken)
			actor := browser
			switch change {
			case "target-token":
				s.st.DB.Model(&target).Update("sub_token", "changed-subscription-token")
			case "new-order":
				s.st.DB.Create(&store.Order{ID: "new-order", UserID: current.ID, Status: "pending"})
			case "other-session":
				actor = bindingBrowser(t, s, srv.URL, current, true)
			}
			st, body := actor.req("POST", "/u/subscription/bind", map[string]any{"ticket": preview["ticket"]}, true)
			if st != 409 && !(change == "other-session" && st == 400) {
				t.Fatal(st, body)
			}
			var got store.User
			s.st.DB.First(&got, "id = ?", current.ID)
			if got.Email != current.Email || got.Status == "disabled" {
				t.Fatal("failed confirmation changed source")
			}
			if change == "other-session" {
				if st, body := browser.req("POST", "/u/subscription/bind", map[string]any{"ticket": preview["ticket"]}, true); st != 200 {
					t.Fatal("wrong browser consumed confirmation", st, body)
				}
			}
		})
	}
}

func TestSubscriptionBindingAllowsLiveTrafficBetweenPreviewAndConfirm(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	current := bindingPlaceholder(t, s, "registration", "registration@example.com", 0)
	target := bindingSubscriber(t, s, "live-subscription")
	browser := bindingBrowser(t, s, srv.URL, current, true)
	preview := bindingPreview(t, browser, target.SubToken)
	target.UsedDown += 123
	if err := s.st.DB.Model(&target).Update("used_down", target.UsedDown).Error; err != nil {
		t.Fatal(err)
	}
	if code, body := browser.req("POST", "/u/subscription/bind", map[string]any{"ticket": preview["ticket"]}, true); code != 200 {
		t.Fatal("live traffic caused a false conflict", code, body)
	}
	assertBindingUnchanged(t, s, target)
}

func TestSubscriptionBindingConcurrentClaims(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	target := bindingSubscriber(t, s, "subscriber")
	a := bindingPlaceholder(t, s, "a", "a@example.com", 0)
	b := bindingPlaceholder(t, s, "b", "b@example.com", 0)
	clients := []*portalClient{bindingBrowser(t, s, srv.URL, a, true), bindingBrowser(t, s, srv.URL, b, true)}
	previews := []map[string]any{bindingPreview(t, clients[0], target.SubToken), bindingPreview(t, clients[1], target.SubToken)}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i, client := range clients {
		wg.Add(1)
		go func(i int, client *portalClient) {
			defer wg.Done()
			status, _ := client.req("POST", "/u/subscription/bind", map[string]any{"ticket": previews[i]["ticket"]}, true)
			codes <- status
		}(i, client)
	}
	wg.Wait()
	close(codes)
	success, conflicts := 0, 0
	for code := range codes {
		if code == 200 {
			success++
		} else if code == 409 {
			conflicts++
		} else {
			t.Fatal("unexpected concurrent result", code)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal(success, conflicts)
	}
	assertBindingUnchanged(t, s, target)
}

func TestMailBindingReusesRegisteredAccount(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	mail := useMail(t, s)
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("portal_email_register", "false")
	current := bindingSubscriber(t, s, "subscriber")
	current.TelegramID = 4242
	s.st.DB.Model(&current).Update("telegram_id", 4242)
	registered := bindingPlaceholder(t, s, "registered", "existing@example.com", 0)
	old := bindingBrowser(t, s, srv.URL, registered, true)
	browser := bindingBrowser(t, s, srv.URL, current, false)
	if st, body := browser.req("POST", "/u/email/code", map[string]any{"email": registered.Email, "purpose": "bind"}, true); st != 200 {
		t.Fatal(st, body)
	}
	if st, _ := browser.req("POST", "/u/email/verify", map[string]any{"email": registered.Email, "purpose": "bind", "code": "bad"}, true); st != 400 {
		t.Fatal(st)
	}
	var untouched store.User
	s.st.DB.First(&untouched, "id = ?", current.ID)
	if untouched.Email != "" {
		t.Fatal("unverified email changed ownership")
	}
	code := lastCode(t, mail, registered.Email, 1)
	if st, body := browser.req("POST", "/u/email/verify", map[string]any{"email": registered.Email, "purpose": "bind", "code": code}, true); st != 200 {
		t.Fatal(st, body)
	}
	var kept store.User
	s.st.DB.First(&kept, "id = ?", current.ID)
	if kept.Email != registered.Email || kept.TelegramID != 4242 {
		t.Fatal("registered identity not linked")
	}
	assertBindingUnchanged(t, s, current)
	if st, _ := old.req("GET", "/u/sessions", nil, false); st != 401 {
		t.Fatal("retired mail session survived")
	}
	if st, _ := browser.req("GET", "/u/sessions", nil, false); st != 200 {
		t.Fatal("current browser lost access")
	}
}

func TestTelegramBindingReusesUnprovisionedAccount(t *testing.T) {
	s := newTestServer(t)
	current := bindingPlaceholder(t, s, "registered", "linked@example.com", 4242)
	target := bindingSubscriber(t, s, "subscriber")
	kept, err := s.bindTelegramSubscription(4242, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.ID != target.ID || kept.TelegramID != 4242 || kept.Email != current.Email {
		t.Fatal("Telegram account was not linked")
	}
	assertBindingUnchanged(t, s, target)
	other := bindingSubscriber(t, s, "other")
	if _, err := s.bindTelegramSubscription(4242, other.ID); err == nil {
		t.Fatal("Telegram silently abandoned another paid account")
	}
	if _, err := s.bindTelegramSubscription(7777, target.ID); err == nil {
		t.Fatal("another Telegram stole the binding")
	}
	var count int64
	s.st.DB.Model(&store.User{}).Where("telegram_id = ?", 4242).Count(&count)
	if count != 1 {
		t.Fatal("duplicate Telegram binding", count)
	}
}

func TestSameAccountBindingIsIdempotent(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	u := bindingSubscriber(t, s, "self")
	u.Email = "self@example.com"
	s.st.DB.Model(&u).Update("email", u.Email)
	browser := bindingBrowser(t, s, srv.URL, u, true)
	p := bindingPreview(t, browser, u.SubToken)
	if p["sameAccount"] != true {
		t.Fatal(p)
	}
	if st, body := browser.req("POST", "/u/subscription/bind", map[string]any{"ticket": p["ticket"]}, true); st != 200 {
		t.Fatal(st, body)
	}
	assertBindingUnchanged(t, s, u)
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), u.SubToken) || strings.Contains(string(raw), u.Password) {
		t.Fatal("preview exposed credentials")
	}
}

func TestAccountAssociationCheckoutRace(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	current := bindingPlaceholder(t, s, "checkout", "checkout@example.com", 0)
	target := bindingSubscriber(t, s, "paid")
	browser := bindingBrowser(t, s, srv.URL, current, true)
	preview := bindingPreview(t, browser, target.SubToken)
	start := make(chan struct{})
	binding := make(chan int, 1)
	order := make(chan error, 1)
	go func() {
		<-start
		code, _ := browser.req("POST", "/u/subscription/bind", map[string]any{"ticket": preview["ticket"]}, true)
		binding <- code
	}()
	go func() {
		<-start
		order <- s.createAccountOrder(&store.Order{ID: "racing-checkout", UserID: current.ID, Status: "pending"})
	}()
	close(start)
	code, err := <-binding, <-order
	if (code == 200 && err == nil) || (code != 200 && code != 409) {
		t.Fatal("checkout and account retirement both succeeded", code, err)
	}
	if code == 409 && err != nil {
		t.Fatal("both operations failed", code, err)
	}
	assertBindingUnchanged(t, s, target)
}
