package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestShouldStepDown pins the pure handover decision: only a live request
// naming another instance makes the current leader yield.
func TestShouldStepDown(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		holder    string
		expiresAt time.Time
		self      string
		leader    bool
		want      bool
	}{
		{"live request from another instance", "other", now.Add(time.Second), "self", true, true},
		{"no request recorded", "", now.Add(time.Minute), "self", true, false},
		{"request already expired", "other", now.Add(-time.Second), "self", true, false},
		{"request expiring exactly now", "other", now, "self", true, false},
		{"own request", "self", now.Add(time.Minute), "self", true, false},
		{"not the leader", "other", now.Add(time.Minute), "self", false, false},
	}
	for _, tc := range cases {
		if got := shouldStepDown(tc.holder, tc.expiresAt, tc.self, now, tc.leader); got != tc.want {
			t.Errorf("%s: shouldStepDown = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStepdownCooldown: after a yield the instance stays out of the lease
// race until the cooldown instant passes.
func TestStepdownCooldown(t *testing.T) {
	s := &Server{}
	now := time.Now()
	if s.inStepdownCooldown(now) {
		t.Fatal("no yield yet, but in cooldown")
	}
	s.stepdownUntil.Store(now.Add(time.Minute).UnixNano())
	if !s.inStepdownCooldown(now) {
		t.Fatal("inside the cooldown window")
	}
	if s.inStepdownCooldown(now.Add(2 * time.Minute)) {
		t.Fatal("cooldown over, but still cooling down")
	}
	s.stepdownUntil.Store(now.Add(-time.Minute).UnixNano())
	if s.inStepdownCooldown(now) {
		t.Fatal("a past cooldown instant must not block")
	}
}

// TestHATakeLeaderStandalone: a single-instance (SQLite) panel is always
// its own leader; the takeover endpoint refuses and writes nothing.
func TestHATakeLeaderStandalone(t *testing.T) {
	s, _, c := newAdminClient(t)
	code, out := c.do("POST", "/ha/take-leader", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("take-leader on SQLite: %d %v", code, out)
	}
	var n int64
	s.st.DB.Model(&store.ClusterLease{}).Where("name = ?", stepdownLeaseName).Count(&n)
	if n != 0 {
		t.Fatalf("a refused request must not record a stepdown row: %d", n)
	}
}

// TestHATakeLeaderRequiresSudo: taking over leadership is a sensitive
// action, so it needs a fresh sudo like the other HA writes.
func TestHATakeLeaderRequiresSudo(t *testing.T) {
	s, _, c := newAdminClient(t)
	h, _ := bcrypt.GenerateFromPassword([]byte("pw4"), bcrypt.MinCost)
	s.st.DB.Create(&store.Admin{ID: "a4", Username: "fourth", PasswordHash: string(h)})
	plain := &apiClient{t: t, base: c.base}
	code, out := plain.do("POST", "/auth/login", map[string]string{"username": "fourth", "password": "pw4"})
	if code != 200 {
		t.Fatalf("login %d %v", code, out)
	}
	plain.tok = out["token"].(string)
	code, out = plain.do("POST", "/ha/take-leader", nil)
	if code != http.StatusForbidden || out["sudoRequired"] != true {
		t.Fatalf("non-sudo take-leader: %d %v", code, out)
	}
}

// callTakeLeader invokes the takeover handler directly (the cluster test
// servers run no HTTP listener).
func callTakeLeader(t *testing.T, s *Server) int {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/ha/take-leader", nil)
	s.haTakeLeader(c)
	return w.Code
}

// TestLeaderTakeover walks the whole handover on PostgreSQL: the standby
// asks, the leader yields on its next lease round without grabbing the
// lease straight back, and the requester ends up leading.
func TestLeaderTakeover(t *testing.T) {
	ss := clusterServers(t, pgOnly(t), 2)
	a, b := ss[0], ss[1]
	if !a.isLeader() || b.isLeader() {
		t.Fatalf("leaders: a=%v b=%v", a.isLeader(), b.isLeader())
	}
	// The leader itself has nothing to take over.
	if code := callTakeLeader(t, a); code != http.StatusBadRequest {
		t.Fatalf("leader take-leader: %d", code)
	}
	// A running sync upgrade refuses the request: the orchestration is
	// mid-flight and does not need a leadership change on top.
	opID, err := b.startSyncUpdate("v0.1.20", false)
	if err != nil {
		t.Fatal(err)
	}
	if code := callTakeLeader(t, b); code != http.StatusConflict {
		t.Fatalf("take-leader during a sync update: %d", code)
	}
	if err := b.st.DB.Delete(&store.ClusterOperation{}, "id = ?", opID).Error; err != nil {
		t.Fatal(err)
	}
	// The standby's request is accepted and lands as a stepdown row
	// naming the requester.
	if code := callTakeLeader(t, b); code != http.StatusOK {
		t.Fatalf("standby take-leader: %d", code)
	}
	var row store.ClusterLease
	if err := b.st.DB.Where("name = ?", stepdownLeaseName).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Holder != b.procID {
		t.Fatalf("stepdown holder: %q, want the requester %q", row.Holder, b.procID)
	}
	// The leader honors the request on its next round and consumes it.
	a.updateLease()
	if a.isLeader() {
		t.Fatal("leader did not yield")
	}
	var n int64
	a.st.DB.Model(&store.ClusterLease{}).Where("name = ?", stepdownLeaseName).Count(&n)
	if n != 0 {
		t.Fatalf("stepdown row not consumed: %d", n)
	}
	// The freed lease stays free for the requester: the yielder's next
	// round must not grab it back inside the cooldown.
	a.updateLease()
	if a.isLeader() {
		t.Fatal("yielding instance re-grabbed the lease during its cooldown")
	}
	// The requester claims the lease on its own round.
	b.updateLease()
	if !b.isLeader() {
		t.Fatal("requester did not take over")
	}
	a.updateLease()
	if a.isLeader() {
		t.Fatal("old leader took a live lease back")
	}
}
