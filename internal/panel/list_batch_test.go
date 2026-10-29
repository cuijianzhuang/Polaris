package panel

import (
	"fmt"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// countQueries counts SELECTs while fn runs.
func countQueries(t *testing.T, db *gorm.DB, fn func()) int64 {
	t.Helper()
	var n atomic.Int64
	name := fmt.Sprintf("test:count:%p", &n)
	var on atomic.Bool
	if err := db.Callback().Query().After("gorm:query").Register(name, func(*gorm.DB) {
		if on.Load() {
			n.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(name) })
	on.Store(true)
	fn()
	on.Store(false)
	return n.Load()
}

// List pages use a fixed number of queries however many rows they show, and
// still report each row's members and counts.
func TestListPagesQueryCountIsFlat(t *testing.T) {
	s, _, c := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "n1"})
	db.Create(&store.Inbound{ID: "i1", NodeID: "n1", Tag: "t1", Port: 1001, TemplateID: "tp1"})
	db.Create(&store.InboundTemplate{ID: "tp1", Name: "tp1"})
	db.Create(&store.CDNProfile{ID: "cdn1", Name: "cdn1"})
	db.Create(&store.Endpoint{ID: "e1", InboundID: "i1", CDNProfileID: "cdn1"})
	db.Create(&store.User{ID: "u1", Username: "u1", SubToken: "s1", PlanID: "p0"})

	addRows := func(from, to int) {
		for i := from; i < to; i++ {
			g := fmt.Sprintf("g%d", i)
			db.Create(&store.Group{ID: g, Name: g})
			db.Create(&store.GroupInbound{GroupID: g, InboundID: "i1"})
			db.Create(&store.GroupNode{GroupID: g, NodeID: "n1"})
			p := fmt.Sprintf("p%d", i)
			db.Create(&store.Group{ID: p, Name: p, Plan: true})
			db.Create(&store.GroupInbound{GroupID: p, InboundID: "i1"})
			db.Create(&store.PlanNodeRate{PlanID: p, NodeID: "n1", Rate: 2})
			tp := fmt.Sprintf("tpx%d", i)
			db.Create(&store.InboundTemplate{ID: tp, Name: tp})
			cd := fmt.Sprintf("cdx%d", i)
			db.Create(&store.CDNProfile{ID: cd, Name: cd})
		}
		db.Create(&store.UserGroup{UserID: "u1", GroupID: "g0"})
	}
	paths := []string{"/groups", "/plans", "/templates", "/cdn-profiles"}
	measure := func() map[string]int64 {
		out := map[string]int64{}
		for _, p := range paths {
			out[p] = countQueries(t, db, func() {
				if code, _ := c.doList("GET", p); code != 200 {
					t.Fatalf("%s: %d", p, code)
				}
			})
		}
		return out
	}
	addRows(0, 2)
	measure() // warm caches (settings are cached for a while): count per-row queries only
	few := measure()
	addRows(2, 12)
	many := measure()
	for _, p := range paths {
		if many[p] != few[p] {
			t.Errorf("%s: %d queries for 2 rows, %d for 12", p, few[p], many[p])
		}
	}

	// The batched values are still right.
	_, groups := c.doList("GET", "/groups")
	for _, raw := range groups {
		g := raw.(map[string]any)
		if g["id"] == "g0" {
			if fmt.Sprint(g["inboundIds"]) != "[i1]" || fmt.Sprint(g["nodeIds"]) != "[n1]" || g["userCount"] != float64(1) {
				t.Fatalf("group g0 = %v", g)
			}
		}
	}
	_, plans := c.doList("GET", "/plans")
	for _, raw := range plans {
		p := raw.(map[string]any)
		if p["id"] == "p0" {
			if fmt.Sprint(p["inboundIds"]) != "[i1]" || fmt.Sprint(p["nodeIds"]) != "[]" || p["userCount"] != float64(1) || fmt.Sprint(p["nodeRates"]) != "map[n1:2]" {
				t.Fatalf("plan p0 = %v", p)
			}
		}
	}
	_, tps := c.doList("GET", "/templates")
	for _, raw := range tps {
		tp := raw.(map[string]any)
		if tp["id"] == "tp1" && (tp["inbounds"] != float64(1) || tp["nodes"] != float64(1)) {
			t.Fatalf("template tp1 = %v", tp)
		}
	}
	_, cdns := c.doList("GET", "/cdn-profiles")
	for _, raw := range cdns {
		cd := raw.(map[string]any)
		if cd["id"] == "cdn1" && cd["inbounds"] != float64(1) {
			t.Fatalf("cdn1 = %v", cd)
		}
	}
}
