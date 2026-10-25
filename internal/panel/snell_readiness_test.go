package panel

import (
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"testing"
)

func TestSnellReadinessRequiresActualAgentCoreAndCounters(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "snell-node", Name: "Snell node"})
	check := func(want bool) {
		t.Helper()
		code, out := c.do("GET", "/nodes/snell-node/snell-readiness", nil)
		if code != 200 || out["ready"] != want {
			t.Fatal(code, out)
		}
		if !want && len(out["reasons"].([]any)) == 0 {
			t.Fatal("missing actionable reason", out)
		}
	}
	check(false)
	s.st.DB.Model(&store.Node{}).Where("id = ?", "snell-node").Update("agent_features", store.NewJSON([]string{model.SnellUsersFeature, model.SnellPSKFeature}))
	cr := store.Core{ID: "snell-core", NodeID: "snell-node", Type: model.CoreSingbox, Version: "1.14.4", RunningVersion: "1.13.5", Status: "running"}
	s.st.DB.Create(&cr)
	check(false)
	s.st.DB.Model(&cr).Update("running_version", "1.14.4")
	check(true)
	s.st.DB.Model(&cr).Update("no_user_traffic", true)
	check(false)
	s.st.DB.Model(&cr).Updates(map[string]any{"no_user_traffic": false, "running_version": "1.14.5-alpha.1"})
	check(false)
	s.st.DB.Model(&cr).Update("running_version", "1.14.4")
	s.st.DB.Model(&store.Node{}).Where("id = ?", "snell-node").Update("revoked", true)
	check(false)
}
