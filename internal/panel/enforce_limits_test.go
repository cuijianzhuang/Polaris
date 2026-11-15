package panel

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

// enforceLimits reads only rows that can change. Every transition it must
// perform therefore needs a row the WHERE clause keeps: losing one leaves an
// account in the wrong state forever, with no error to show for it.
func TestEnforceLimitsKeepsEveryTransition(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC()
	past := now.Add(-2 * time.Hour)
	future := now.Add(24 * time.Hour)
	mk := func(id string, f func(*store.User)) string {
		u := store.User{ID: id, Username: id, UUID: store.NewID(), Password: "p", SubToken: store.NewID(),
			Status: "active", OnlineIPs: store.NewJSON([]string{})}
		f(&u)
		if err := s.st.DB.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Each case must be selected by the predicate and end in the state named.
	expiring := mk("expire", func(u *store.User) { u.ExpireAt = &past })
	limiting := mk("limit", func(u *store.User) { u.TrafficLimit = 100; u.UsedUp = 60; u.UsedDown = 40 })
	renewing := mk("renew", func(u *store.User) { u.ExpireAt = &past; u.RenewDays = 30 })
	fallback := mk("fallback", func(u *store.User) { u.ExpireAt = &past; u.PlanID = store.NewID() })
	// Recoveries must also be selected, even with no expiry or renewal plan.
	recoveredQuota := mk("quota-restored", func(u *store.User) { u.Status = "limited"; u.TrafficLimit = 100 })
	recoveredExpiry := mk("expiry-extended", func(u *store.User) { u.Status = "expired"; u.ExpireAt = &future })
	removedExpiry := mk("expiry-removed", func(u *store.User) { u.Status = "expired" })
	stillLimited := mk("still-limited", func(u *store.User) { u.Status = "limited"; u.TrafficLimit = 100; u.UsedUp = 100 })
	disabled := mk("disabled", func(u *store.User) { u.Status = "disabled"; u.ExpireAt = &past })
	service := mk("service", func(u *store.User) { u.IsService = true; u.ExpireAt = &past })
	// Already correct or unrelated: must be left alone.
	steady := mk("steady", func(u *store.User) { u.ExpireAt = &future; u.TrafficLimit = 1 << 40 })
	noExpiry := mk("noexpiry", func(u *store.User) { u.TrafficLimit = 1 << 40 })
	unlimited := mk("unlimited", func(u *store.User) { u.ExpireAt = &future; u.TrafficLimit = 0 })

	// API/import callers may persist explicit offsets. SQL must compare
	// instants instead of SQLite's lexical representation against UTC.
	shiftedPast := past.In(time.FixedZone("east", 8*60*60))
	offsetExpiry := mk("offset-expiry", func(u *store.User) { u.ExpireAt = &shiftedPast })
	nullCounter := mk("null-counter", func(u *store.User) { u.TrafficLimit = 100; u.UsedDown = 100 })
	if err := s.st.DB.Model(&store.User{}).Where("id = ?", nullCounter).Update("used_up", nil).Error; err != nil {
		t.Fatal(err)
	}
	s.enforceLimits()

	want := map[string]string{
		offsetExpiry:    "expired",
		nullCounter:     "limited",
		expiring:        "expired",
		limiting:        "limited",
		renewing:        "active",  // renewed, expiry moved into the future
		fallback:        "expired", // no fallback plan configured: plain expiry
		steady:          "active",
		noExpiry:        "active",
		unlimited:       "active",
		recoveredQuota:  "active",
		recoveredExpiry: "active",
		removedExpiry:   "active",
		stillLimited:    "limited",
		disabled:        "disabled",
		service:         "active",
	}
	for id, status := range want {
		var got store.User
		if err := s.st.DB.First(&got, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		if got.Status != status {
			t.Errorf("%s: status=%s, want %s", id, got.Status, status)
		}
	}
	var renewed store.User
	s.st.DB.First(&renewed, "id = ?", renewing)
	if renewed.RenewCount != 1 || renewed.ExpireAt == nil || !renewed.ExpireAt.After(now) || renewed.Used() != 0 {
		t.Fatalf("renewal skipped or incomplete: %+v", renewed)
	}
	var limited store.User
	s.st.DB.First(&limited, "id = ?", limiting)
	if limited.Used() != limited.TrafficLimit {
		t.Fatalf("limit case not set up as expected: %+v", limited)
	}
}

// The predicate must not turn a large users table into a per-tick scan: an
// idle population is read once and yields no work.
func TestEnforceLimitsIgnoresSteadyPopulation(t *testing.T) {
	s := newTestServer(t)
	future := time.Now().Add(30 * 24 * time.Hour)
	users := make([]store.User, 300)
	for i := range users {
		users[i] = store.User{ID: fmt.Sprintf("s%03d", i), Username: fmt.Sprintf("s%03d", i), UUID: store.NewID(),
			Password: "p", SubToken: store.NewID(), Status: "active", TrafficLimit: 1 << 40, ExpireAt: &future}
	}
	if err := s.st.DB.CreateInBatches(&users, 100).Error; err != nil {
		t.Fatal(err)
	}
	var reads int
	var loaded int64
	const callback = "test:enforce-steady-reads"
	if err := s.st.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			reads++
			loaded += tx.RowsAffected
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Query().Remove(callback) })
	s.enforceLimits()
	s.st.DB.Callback().Query().Remove(callback)
	// The scan, the (empty) IP ban sweep: no per-user work.
	if reads != 2 || loaded != 0 {
		t.Fatalf("steady population: reads=%d loaded=%d, want 2 reads loading no users", reads, loaded)
	}
}

func TestEnforceLimitsPreservesConcurrentBanExtension(t *testing.T) {
	s := newTestServer(t)
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	u := store.User{ID: "extended-ban", Username: "extended-ban", UUID: store.NewID(), SubToken: store.NewID(), Status: "active", BannedUntil: &past}
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	const callback = "test:extend-ban-after-read"
	interleaved := false
	if err := s.st.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" && slices.Equal(tx.Statement.Selects, []string{"id"}) && !interleaved {
			interleaved = true
			if err := s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("banned_until", future).Error; err != nil {
				t.Error(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Query().Remove(callback) })
	s.enforceLimits()
	s.st.DB.Callback().Query().Remove(callback)
	var got store.User
	if err := s.st.DB.First(&got, "id = ?", u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !interleaved || got.BannedUntil == nil || !got.BannedUntil.Equal(future) {
		t.Fatalf("extended ban lost: interleaved=%v until=%v", interleaved, got.BannedUntil)
	}
}

func TestEnforceLimitsRetriesFailedUnban(t *testing.T) {
	s := newTestServer(t)
	past := time.Now().Add(-time.Minute)
	u := store.User{ID: "retry-ban", Username: "retry-ban", UUID: store.NewID(), SubToken: store.NewID(), Status: "active", BannedUntil: &past}
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	const callback = "test:fail-unban-write"
	injected := false
	if err := s.st.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if m, ok := tx.Statement.Dest.(map[string]any); tx.Statement.Table == "users" && ok {
			if v, exists := m["banned_until"]; exists && v == nil {
				injected = true
				tx.AddError(errors.New("injected unban failure"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Update().Remove(callback) })
	s.enforceLimits()
	s.st.DB.Callback().Update().Remove(callback)
	var got store.User
	if err := s.st.DB.First(&got, "id = ?", u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !injected || got.BannedUntil == nil {
		t.Fatal("failed unban did not retain ban")
	}
	s.enforceLimits()
	got = store.User{}
	if err := s.st.DB.First(&got, "id = ?", u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.BannedUntil != nil {
		t.Fatal("next tick failed to retry unban")
	}
}

// A wave of temporary bans must be lifted in a few statements, and a ban that
// is still running must survive.
func TestEnforceLimitsLiftsExpiredBansInBatches(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	total := settleBatch + 20 // more than one chunk
	users := make([]store.User, total)
	for i := range users {
		id := fmt.Sprintf("b%03d", i)
		until := past
		if i == total-1 {
			until = future // still banned
		}
		users[i] = store.User{ID: id, Username: id, UUID: store.NewID(), Password: "p",
			SubToken: store.NewID(), Status: "active", BannedUntil: &until}
	}
	if err := s.st.DB.CreateInBatches(&users, 200).Error; err != nil {
		t.Fatal(err)
	}
	var banWrites int
	const callback = "test:enforce-unban-writes"
	if err := s.st.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if m, ok := tx.Statement.Dest.(map[string]any); ok {
			if v, ok := m["banned_until"]; ok && v == nil {
				banWrites++
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Update().Remove(callback) })

	s.enforceLimits()
	s.st.DB.Callback().Update().Remove(callback)
	if banWrites != 2 {
		t.Fatalf("lifting %d bans took %d statements, want 2 batches", total-1, banWrites)
	}
	var stillBanned, lifted int64
	s.st.DB.Model(&store.User{}).Where("banned_until IS NOT NULL").Count(&stillBanned)
	s.st.DB.Model(&store.User{}).Where("banned_until IS NULL").Count(&lifted)
	if stillBanned != 1 || lifted != int64(total-1) {
		t.Fatalf("bans: %d still set, %d lifted, want 1 and %d", stillBanned, lifted, total-1)
	}
}
