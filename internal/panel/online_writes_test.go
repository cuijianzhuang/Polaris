package panel

import (
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

// A node reports every 20 seconds. A steady report must not rewrite every
// user row: only a changed IP snapshot is written, and online_at is refreshed
// with one statement for the whole report.
func TestOnlineIPReportWritesOnlyChanges(t *testing.T) {
	s := newTestServer(t)
	users := []store.User{
		{ID: "u1", Username: "u1", UUID: store.NewID(), Password: "p", SubToken: store.NewID(), Status: "active", OnlineIPs: store.NewJSON([]string{"203.0.113.1"})},
		{ID: "u2", Username: "u2", UUID: store.NewID(), Password: "p", SubToken: store.NewID(), Status: "active", OnlineIPs: store.NewJSON([]string{"203.0.113.2"})},
		{ID: "u3", Username: "u3", UUID: store.NewID(), Password: "p", SubToken: store.NewID(), Status: "active", OnlineIPs: store.NewJSON([]string{})},
	}
	if err := s.st.DB.Create(&store.Node{ID: "n", Name: "n"}).Error; err != nil {
		t.Fatal(err)
	}
	for i := range users {
		if err := s.st.DB.Create(&users[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	allowed := map[string]bool{"u1": true, "u2": true, "u3": true}
	s.nodeUserSets.mu.Lock()
	s.nodeUserSets.m = map[string]nodeUsersEntry{"n": {set: allowed, own: allowed}}
	s.nodeUserSets.mu.Unlock()

	var updates atomic.Int32
	const callback = "test:online-ip-update-count"
	if err := s.st.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			updates.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Update().Remove(callback) })
	past := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	if err := s.st.DB.Model(&store.User{}).Where("1 = 1").Update("online_at", past).Error; err != nil {
		t.Fatal(err)
	}
	report := func(ips map[string][]string) {
		t.Helper()
		m := &polarisv1.OnlineIPs{}
		for _, id := range []string{"u1", "u2", "u3"} {
			if list, ok := ips[id]; ok {
				m.Users = append(m.Users, &polarisv1.UserIPs{UserId: id, Ips: list})
			}
		}
		s.handleOnlineIPs("n", m)
	}
	user := func(id string) store.User {
		t.Helper()
		var u store.User
		if err := s.st.DB.First(&u, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		return u
	}

	// Same snapshot for every user: one statement refreshes online_at and no
	// per-user row is touched.
	updates.Store(0)
	report(map[string][]string{"u1": {"203.0.113.1"}, "u2": {"203.0.113.2"}})
	if n := updates.Load(); n != 1 {
		t.Fatalf("steady report issued %d user writes, want 1", n)
	}
	u1 := user("u1")
	if u1.OnlineAt == nil || !u1.OnlineAt.After(past) || !slices.Equal(u1.OnlineIPs.V, []string{"203.0.113.1"}) {
		t.Fatalf("steady report did not refresh online state: %+v", u1)
	}

	// u1 moved to a new address: exactly one extra write, plus the batch.
	updates.Store(0)
	report(map[string][]string{"u1": {"203.0.113.9"}, "u2": {"203.0.113.2"}})
	if n := updates.Load(); n != 2 {
		t.Fatalf("changed address issued %d user writes, want 2", n)
	}
	if u1 = user("u1"); !slices.Equal(u1.OnlineIPs.V, []string{"203.0.113.9"}) {
		t.Fatalf("changed address not stored: %+v", u1)
	}
	// u1 disconnects; u3 was already empty and must not be written at all.
	refreshed := *user("u1").OnlineAt
	updates.Store(0)
	report(map[string][]string{"u1": nil, "u3": nil})
	if n := updates.Load(); n != 1 {
		t.Fatalf("disconnect issued %d user writes, want 1 (u1 clears, u3 unchanged)", n)
	}
	if u1 = user("u1"); len(u1.OnlineIPs.V) != 0 || u1.OnlineAt == nil || !u1.OnlineAt.Equal(refreshed) {
		t.Fatalf("disconnect cleared the snapshot but touched online_at: %+v", u1)
	}
	if u3 := user("u3"); u3.OnlineAt == nil || !u3.OnlineAt.Equal(past) {
		t.Fatalf("empty report marked an idle user online: %+v", u3)
	}

	// A NULL column reads as empty just like a stored []: an empty report must
	// not start rewriting that row on every 20-second report.
	if err := s.st.DB.Model(&store.User{}).Where("id = ?", "u3").Update("online_ips", nil).Error; err != nil {
		t.Fatal(err)
	}
	updates.Store(0)
	report(map[string][]string{"u1": nil, "u3": nil})
	if n := updates.Load(); n != 0 {
		t.Fatalf("NULL snapshot rewritten %d times by an empty report", n)
	}
}

// onlineWriteRecorder counts user-table update statements, separating IP
// snapshot writes from the batched online_at / ban writes.
type onlineWriteRecorder struct {
	total, ips atomic.Int32
}

func (r *onlineWriteRecorder) attach(t *testing.T, s *Server) {
	t.Helper()
	const callback = "test:online-write-recorder"
	if err := s.st.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "users" {
			return
		}
		r.total.Add(1)
		// GORM turns both the map form and the column+CASE form into a Dest
		// map; the snapshot write is the one that assigns online_ips a value
		// that is not a bound parameter slice.
		if m, ok := tx.Statement.Dest.(map[string]any); ok {
			if _, ok := m["online_ips"]; ok {
				r.ips.Add(1)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Update().Remove(callback) })
	r.reset()
}

func (r *onlineWriteRecorder) reset() { r.total.Store(0); r.ips.Store(0) }

// Enforcement must not depend on the snapshot changing: an over-limit user
// whose IP list is already stored is still banned and alerted.
func TestOnlineIPReportEnforcesOnUnchangedSnapshot(t *testing.T) {
	s := newTestServer(t)
	s.st.SetSetting("ip_ban_minutes", "5")
	u := store.User{ID: "u", Username: "over", UUID: store.NewID(), Password: "p", SubToken: store.NewID(),
		Status: "active", IPLimit: 1, OnlineIPs: store.NewJSON([]string{"203.0.113.1", "203.0.113.2"})}
	if err := s.st.DB.Create(&store.Node{ID: "n", Name: "n"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"u": true}
	s.nodeUserSets.mu.Lock()
	s.nodeUserSets.m = map[string]nodeUsersEntry{"n": {set: allowed, own: allowed}}
	s.nodeUserSets.mu.Unlock()
	var rec onlineWriteRecorder
	rec.attach(t, s)

	// The stored list already matches the report: no snapshot write, but the
	// batch refresh and the ban must still happen.
	s.handleOnlineIPs("n", &polarisv1.OnlineIPs{Users: []*polarisv1.UserIPs{{
		UserId: "u", Ips: []string{"203.0.113.1", "203.0.113.2"}}}})
	if rec.ips.Load() != 0 {
		t.Fatalf("unchanged snapshot rewritten %d times", rec.ips.Load())
	}
	if n := rec.total.Load(); n != 2 {
		t.Fatalf("steady over-limit report issued %d writes, want online_at + ban: 2", n)
	}
	var got store.User
	if err := s.st.DB.First(&got, "id = ?", "u").Error; err != nil {
		t.Fatal(err)
	}
	if got.BannedUntil == nil || !slices.Equal(got.OnlineIPs.V, []string{"203.0.113.1", "203.0.113.2"}) {
		t.Fatalf("over-limit user not banned on unchanged snapshot: %+v", got)
	}
}

// More live users than one batch: the online_at refresh must stay within the
// statement parameter limit instead of growing one bind per user.
func TestOnlineIPReportBatchChunksOnlineAt(t *testing.T) {
	s := newTestServer(t)
	if err := s.st.DB.Create(&store.Node{ID: "n", Name: "n"}).Error; err != nil {
		t.Fatal(err)
	}
	total := settleBatch + 1
	users := make([]store.User, total)
	allowed := make(map[string]bool, total)
	m := &polarisv1.OnlineIPs{}
	for i := range users {
		id := fmt.Sprintf("u%03d", i)
		users[i] = store.User{ID: id, Username: id, UUID: store.NewID(), Password: "p", SubToken: store.NewID(),
			Status: "active", OnlineIPs: store.NewJSON([]string{"203.0.113.7"})}
		allowed[id] = true
		m.Users = append(m.Users, &polarisv1.UserIPs{UserId: id, Ips: []string{"203.0.113.7"}})
	}
	if err := s.st.DB.CreateInBatches(&users, 200).Error; err != nil {
		t.Fatal(err)
	}
	s.nodeUserSets.mu.Lock()
	s.nodeUserSets.m = map[string]nodeUsersEntry{"n": {set: allowed, own: allowed}}
	s.nodeUserSets.mu.Unlock()
	var rec onlineWriteRecorder
	rec.attach(t, s)

	s.handleOnlineIPs("n", m)
	if n := rec.ips.Load(); n != 0 {
		t.Fatalf("unchanged snapshots rewritten %d times", n)
	}
	if n := rec.total.Load(); n != 2 {
		t.Fatalf("online_at refreshed in %d statements for %d users, want 2", n, total)
	}
}

// Many devices connecting at once used to cost one UPDATE per user. The
// snapshots are now written per chunk with a CASE, and every snapshot must
// survive that expression intact, including a cleared one.
func TestOnlineIPReportChunksChangedSnapshots(t *testing.T) {
	s := newTestServer(t)
	if err := s.st.DB.Create(&store.Node{ID: "n", Name: "n"}).Error; err != nil {
		t.Fatal(err)
	}
	total := settleBatch + 1
	users := make([]store.User, total)
	allowed := make(map[string]bool, total)
	m := &polarisv1.OnlineIPs{}
	want := map[string][]string{}
	for i := range users {
		id := fmt.Sprintf("u%03d", i)
		// u000 and the last user hold a stale list that the report clears;
		// everyone else starts empty and gets its first snapshot.
		stored := []string{}
		if i == 0 || i == total-1 {
			stored = []string{"203.0.113.7"}
		}
		users[i] = store.User{ID: id, Username: id, UUID: store.NewID(), Password: "p", SubToken: store.NewID(),
			Status: "active", OnlineIPs: store.NewJSON(stored)}
		allowed[id] = true
		ips := []string{fmt.Sprintf("198.51.100.%d", i%256)}
		if i == 0 || i == total-1 {
			ips = nil // an empty report clears the stored list
		}
		want[id] = append([]string{}, ips...)
		m.Users = append(m.Users, &polarisv1.UserIPs{UserId: id, Ips: ips})
	}
	if err := s.st.DB.CreateInBatches(&users, 200).Error; err != nil {
		t.Fatal(err)
	}
	s.nodeUserSets.mu.Lock()
	s.nodeUserSets.m = map[string]nodeUsersEntry{"n": {set: allowed, own: allowed}}
	s.nodeUserSets.mu.Unlock()
	var rec onlineWriteRecorder
	rec.attach(t, s)

	s.handleOnlineIPs("n", m)
	// 501 users need two chunks: the CASE statement only carries 500 rows.
	if n := rec.ips.Load(); n != 2 {
		t.Fatalf("changed snapshots written in %d statements for %d users, want 2", n, total)
	}
	// Clearing one stored list and the no-op halves prove the batch is not
	// silently merged into one oversized statement.
	var cleared, kept int64
	if err := s.st.DB.Model(&store.User{}).Where("id IN ? AND online_ips = ?", []string{"u000", fmt.Sprintf("u%03d", total-1)}, "[]").Count(&cleared).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Model(&store.User{}).Where("id IN ? AND online_ips <> ?", []string{"u001", "u002"}, "[]").Count(&kept).Error; err != nil {
		t.Fatal(err)
	}
	if cleared != 2 || kept != 2 {
		t.Fatalf("stored snapshots wrong: cleared=%d kept=%d", cleared, kept)
	}
	var got []store.User
	if err := s.st.DB.Order("id").Find(&got, "id IN ?", []string{"u000", "u001"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, u := range got {
		if !slices.Equal(u.OnlineIPs.V, want[u.ID]) {
			t.Fatalf("%s snapshot = %v, want %v", u.ID, u.OnlineIPs.V, want[u.ID])
		}
	}
	var last store.User
	if err := s.st.DB.First(&last, "id = ?", fmt.Sprintf("u%03d", total-1)).Error; err != nil {
		t.Fatal(err)
	}
	if len(last.OnlineIPs.V) != 0 {
		t.Fatalf("cleared snapshot still stored %v", last.OnlineIPs.V)
	}
	// No row may be rewritten by a second identical report.
	rec.reset()
	s.handleOnlineIPs("n", m)
	if n := rec.ips.Load(); n != 0 {
		t.Fatalf("repeated report rewrote %d snapshots", n)
	}
}
