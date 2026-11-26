package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

// grantFixture: a pro panel where user g1 has no plan but an inbound grant
// through an ordinary group, ready for a direct forwarding grant.
func grantFixture(t *testing.T) (*Server, *store.User) {
	t.Helper()
	s := newTestServer(t)
	s.lic.Store(&licenseState{st: license.State{Plan: "pro", Features: []string{license.FeaturePlanAddons}}})
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "hk", Address: "203.0.113.9", PublicIPv4: "203.0.113.9", TrafficRate: 1})
	db.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"})
	db.Create(&store.Inbound{ID: "i1", NodeID: "n1", Tag: "in1", Port: 443, Enabled: true, Core: model.CoreXray,
		Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS})})
	db.Create(&store.Group{ID: "g1", Name: "直授组"})
	db.Create(&store.GroupInbound{GroupID: "g1", InboundID: "i1"})
	u := &store.User{ID: "g1u", Username: "gina", UUID: "x", Password: "p", Status: "active", SubToken: "gina-token-0000000000"}
	db.Create(u)
	db.Create(&store.UserGroup{UserID: "g1u", GroupID: "g1"})
	return s, u
}

func putUser(t *testing.T, s *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/users/"+id, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: id}}
	s.updateUser(c)
	return w
}

func loadUser(t *testing.T, s *Server, id string) store.User {
	t.Helper()
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	return u
}

// A direct grant lands through updateUser with its source marked, is
// audited, can be revoked, and immediately lets a plan-less user forward
// on the nodes they are granted.
func TestFwdDirectGrant(t *testing.T) {
	s, u := grantFixture(t)
	w := putUser(t, s, u.ID, `{"username":"gina","fwd":{"max":3,"portLo":30000,"portHi":30100,"traffic":1000,"rate":2,"speedMbps":8,"resetDay":15,"expireAt":null}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", w.Code, w.Body.String())
	}
	cur := loadUser(t, s, u.ID)
	if cur.FwdSource != "grant" || cur.FwdMax != 3 || cur.FwdPortLo != 30000 || cur.FwdPortHi != 30100 ||
		cur.FwdTraffic != 1000 || cur.FwdRate != 2 || cur.FwdSpeedMbps != 8 || cur.FwdResetDay != 15 || cur.FwdExpireAt != nil {
		t.Fatalf("grant not stored: %+v", cur)
	}
	var audits int64
	s.st.DB.Model(&store.AuditLog{}).Where("action = ? AND target = ?", "forward.grant_update", "gina").Count(&audits)
	if audits != 1 {
		t.Fatalf("grant audits = %d", audits)
	}
	// The granted user can forward on a granted node without any plan.
	r, err := s.createUserForward(context.Background(), &cur, userForwardReq{NodeID: "n1", Target: "1.1.1.1:443", Network: "tcp"})
	if err != nil {
		t.Fatal(err)
	}
	if spec, _ := s.BuildSpec("n1"); len(spec.Forwards) != 1 || spec.Forwards[0].Tag != "fwd-"+r.ID {
		t.Fatalf("grant user's rule not delivered: %+v", spec.Forwards)
	}
	// An independent expiry in the past stops delivery; extending it
	// brings the rule back. Revoking (max 0) clears the source.
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	w = putUser(t, s, u.ID, `{"username":"gina","fwd":{"expireAt":"`+past+`"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("set expiry: %d %s", w.Code, w.Body.String())
	}
	if spec, _ := s.BuildSpec("n1"); len(spec.Forwards) != 0 {
		t.Fatal("forward delivered past its own expiry")
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	putUser(t, s, u.ID, `{"username":"gina","fwd":{"expireAt":"`+future+`"}}`)
	if spec, _ := s.BuildSpec("n1"); len(spec.Forwards) != 1 {
		t.Fatal("forward not back after the expiry was extended")
	}
	putUser(t, s, u.ID, `{"username":"gina","fwd":{"max":0}}`)
	cur = loadUser(t, s, u.ID)
	if cur.FwdSource != "" || cur.FwdMax != 0 {
		t.Fatalf("revoke: source %q max %d", cur.FwdSource, cur.FwdMax)
	}
	if spec, _ := s.BuildSpec("n1"); len(spec.Forwards) != 0 {
		t.Fatal("forward delivered after the grant was revoked")
	}
}

// A plan-managed quota refuses direct edits until the admin converts it;
// edits that omit the fwd group leave it alone entirely.
func TestFwdGrantPlanSourceReadOnly(t *testing.T) {
	s, u := fwdFixture(t)
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("fwd_source", "plan")
	w := putUser(t, s, u.ID, `{"username":"alice","fwd":{"max":9}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("plan quota edited directly: %d %s", w.Code, w.Body.String())
	}
	w = putUser(t, s, u.ID, `{"username":"alice"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("plain edit: %d %s", w.Code, w.Body.String())
	}
	if cur := loadUser(t, s, u.ID); cur.FwdSource != "plan" || cur.FwdMax != 2 {
		t.Fatalf("plain edit touched the quota: %+v", cur.ForwardEntitlement)
	}
	w = putUser(t, s, u.ID, `{"username":"alice","fwd":{"convert":true,"max":9}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("convert: %d %s", w.Code, w.Body.String())
	}
	cur := loadUser(t, s, u.ID)
	if cur.FwdSource != "grant" || cur.FwdMax != 9 || cur.FwdTraffic != 1000 {
		t.Fatalf("converted grant: source %q max %d traffic %d", cur.FwdSource, cur.FwdMax, cur.FwdTraffic)
	}
}

// Validation of the grant group itself.
func TestApplyFwdGrantValidation(t *testing.T) {
	intp := func(v int) *int { return &v }
	u := &store.User{Username: "x"}
	if _, err := applyFwdGrant(u, &userFwdReq{Max: intp(101)}); err == nil {
		t.Fatal("101 rules accepted")
	}
	if _, err := applyFwdGrant(u, &userFwdReq{Max: intp(1), ResetDay: intp(29)}); err == nil {
		t.Fatal("reset day 29 accepted")
	}
	if _, err := applyFwdGrant(u, &userFwdReq{Max: intp(1), PortLo: intp(80), PortHi: intp(9000)}); err == nil {
		t.Fatal("port range below 1024 accepted")
	}
	// Source unknown ("" with terms, the pre-migration shape) can be
	// claimed as a grant.
	u2 := &store.User{Username: "y", ForwardEntitlement: store.ForwardEntitlement{FwdMax: 2}}
	if _, err := applyFwdGrant(u2, &userFwdReq{}); err != nil || u2.FwdSource != "grant" {
		t.Fatalf("claim: %v source %q", err, u2.FwdSource)
	}
}

// The independent reset clears only the forwarding counters on the
// quota's own day; the account path never clears them for such a user.
func TestFwdIndependentReset(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1)
	mk := func(u *store.User) {
		t.Helper()
		if err := s.st.DB.Create(u).Error; err != nil {
			t.Fatal(err)
		}
	}
	// No account reset policy at all: the forwarding quota still cycles.
	mk(&store.User{ID: "a", Username: "ann", UUID: "a", Password: "p", Status: "active", SubToken: "ann-token-00000000000",
		ResetPolicy: "none", UsedUp: 111, UsedDown: 222,
		ForwardEntitlement: store.ForwardEntitlement{FwdMax: 2}, FwdSource: "grant",
		FwdUsed: 500, FwdCapped: true, FwdResetDay: now.Day(), FwdLastResetAt: &yesterday})
	// Account monthly reset due, forwarding reset not due (just stamped):
	// the account path must leave the forwarding counters alone.
	mk(&store.User{ID: "b", Username: "ben", UUID: "b", Password: "p", Status: "active", SubToken: "ben-token-00000000000",
		ResetPolicy: "monthly", ResetDay: now.Day(), LastResetAt: &yesterday, UsedUp: 999,
		ForwardEntitlement: store.ForwardEntitlement{FwdMax: 2}, FwdSource: "grant",
		FwdUsed: 333, FwdResetDay: now.Day(), FwdLastResetAt: &now})
	// Following the account cycle (reset day 0): cleared with the account.
	mk(&store.User{ID: "c", Username: "cat", UUID: "c", Password: "p", Status: "active", SubToken: "cat-token-00000000000",
		ResetPolicy: "monthly", ResetDay: now.Day(), LastResetAt: &yesterday, UsedUp: 888,
		ForwardEntitlement: store.ForwardEntitlement{FwdMax: 2}, FwdSource: "plan", FwdUsed: 444})
	s.resetTrafficCycles()
	a := loadUser(t, s, "a")
	if a.FwdUsed != 0 || a.FwdCapped || a.FwdLastResetAt == nil || a.UsedUp != 111 || a.UsedDown != 222 {
		t.Fatalf("independent reset: %+v", a)
	}
	b := loadUser(t, s, "b")
	if b.UsedUp != 0 || b.FwdUsed != 333 {
		t.Fatalf("account reset cleared the independent quota: proxy %d fwd %d", b.UsedUp, b.FwdUsed)
	}
	c := loadUser(t, s, "c")
	if c.UsedUp != 0 || c.FwdUsed != 0 {
		t.Fatalf("account-following quota: proxy %d fwd %d", c.UsedUp, c.FwdUsed)
	}
	// The stamp holds: a second run does not clear a fresh period again.
	s.st.DB.Model(&store.User{}).Where("id = ?", "a").Update("fwd_used", 7)
	s.resetTrafficCycles()
	if a = loadUser(t, s, "a"); a.FwdUsed != 7 {
		t.Fatalf("second run cleared again: %d", a.FwdUsed)
	}
}

// Applying a plan overwrites a direct grant as a whole — source, reset
// day and expiry included — and the overwrite is audited, never silent.
func TestApplyPlanOverwritesGrant(t *testing.T) {
	s, _ := fwdFixture(t)
	exp := time.Now().AddDate(0, 1, 0)
	g := &store.User{ID: "u9", Username: "ivy", UUID: "z", Password: "p", Status: "active", SubToken: "ivy-token-00000000000",
		ForwardEntitlement: store.ForwardEntitlement{FwdMax: 3, FwdPortLo: 31000, FwdPortHi: 31100, FwdTraffic: 5000},
		FwdSource:          "grant", FwdResetDay: 9, FwdExpireAt: &exp}
	s.st.DB.Create(g)
	var p store.Group
	s.st.DB.First(&p, "id = ?", "p1")
	if _, err := applyPlan(s.st.DB, g, &p, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	cur := loadUser(t, s, "u9")
	if cur.FwdSource != "plan" || cur.FwdMax != 2 || cur.FwdResetDay != 0 || cur.FwdExpireAt != nil {
		t.Fatalf("after plan: source %q max %d resetDay %d expire %v", cur.FwdSource, cur.FwdMax, cur.FwdResetDay, cur.FwdExpireAt)
	}
	var n int64
	s.st.DB.Model(&store.AuditLog{}).Where("action = ?", "forward.grant_overwritten_by_plan").Count(&n)
	if n != 1 {
		t.Fatalf("overwrite audits = %d", n)
	}
	if _, err := applyPlan(s.st.DB, &cur, &p, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.AuditLog{}).Where("action = ?", "forward.grant_overwritten_by_plan").Count(&n)
	if n != 1 {
		t.Fatalf("plan renewal audited as an overwrite: %d", n)
	}
}
