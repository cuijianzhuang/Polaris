package panel

import (
	"sync"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestBalancerFallbackAPI(t *testing.T) {
	s, _, admin := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "relay", Name: "relay"})
	db.Create(&store.Node{ID: "other", Name: "other"})
	db.Create(&store.Core{ID: "xray", NodeID: "relay", Type: model.CoreXray, Version: "26.3.27"})
	db.Create(&store.Outbound{ID: "p", NodeID: "relay", Tag: "proxy", Type: model.OutSOCKS})
	db.Create(&store.Outbound{ID: "remote", NodeID: "other", Tag: "remote", Type: model.OutSOCKS})
	base := "/nodes/relay/outbounds"
	group := func(tag, fallback string, members ...string) map[string]any {
		return map[string]any{"tag": tag, "type": "balancer", "settings": model.OutboundSettings{Members: members, Fallback: fallback, Strategy: model.LBRoundRobin, Interval: "5s"}}
	}
	code, backup := admin.do("POST", base, group("backup", "block", "proxy"))
	if code != 200 {
		t.Fatalf("backup: %d %v", code, backup)
	}
	backupID := backup["id"].(string)
	code, primary := admin.do("POST", base, group("primary", "backup", "direct"))
	if code != 200 {
		t.Fatalf("primary: %d %v", code, primary)
	}
	primaryID := primary["id"].(string)
	for _, body := range []map[string]any{
		group("bad", "bad", "proxy"), group("bad", "remote", "proxy"), group("bad", "proxy", "proxy"),
		group("bad", "backup"), group("bad", "backup", "proxy", "proxy"),
	} {
		if code, out := admin.do("POST", base, body); code != 400 {
			t.Fatalf("invalid group: %d %v", code, out)
		}
	}
	if code, out := admin.do("PUT", base+"/"+backupID, group("backup", "primary", "proxy")); code != 400 {
		t.Fatalf("cycle: %d %v", code, out)
	}
	if code, out := admin.do("PUT", base+"/p", map[string]any{"tag": "proxy", "type": "socks", "detour": "primary"}); code != 400 {
		t.Fatalf("mixed cycle: %d %v", code, out)
	}
	if code, out := admin.do("PUT", base+"/"+backupID, group("renamed", "block", "proxy")); code != 400 {
		t.Fatalf("referenced rename: %d %v", code, out)
	}
	for _, id := range []string{backupID, "p"} {
		if code, out := admin.do("DELETE", base+"/"+id, nil); code != 409 {
			t.Fatalf("referenced delete: %d %v", code, out)
		}
	}
	var saved store.Outbound
	db.First(&saved, "id = ?", backupID)
	modelOut := model.Outbound{Settings: saved.Settings}
	set, _ := modelOut.ParseSettings()
	if set.Fallback != "block" || saved.Tag != "backup" {
		t.Fatalf("failed updates changed backup: %+v", saved)
	}
	if code, out := admin.do("POST", "/nodes/relay/cores", map[string]any{"type": "singbox", "version": "1.13.5"}); code != 409 {
		t.Fatalf("incompatible core install: %d %v", code, out)
	}
	// A mixed-core node must reject a setting one of its cores cannot render.
	db.Create(&store.Core{ID: "sb", NodeID: "relay", Type: model.CoreSingbox, Version: "1.13.5"})
	if code, out := admin.do("POST", base, group("mixed", "backup", "direct")); code != 400 {
		t.Fatalf("sing-box fallback: %d %v", code, out)
	}
	db.Delete(&store.Core{}, "id = ?", "sb")
	if code, out := admin.do("DELETE", base+"/"+primaryID, nil); code != 204 {
		t.Fatalf("primary delete: %d %v", code, out)
	}
	if code, out := admin.do("DELETE", base+"/"+backupID, nil); code != 204 {
		t.Fatalf("backup delete: %d %v", code, out)
	}
}

func TestBalancerFallbackConcurrent(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		db := ss[0].st.DB
		db.Create(&store.Node{ID: "n", Name: "relay"})
		db.Create(&store.Core{ID: "xray", NodeID: "n", Type: model.CoreXray, Version: "26.3.27"})
		for _, tag := range []string{"a", "b"} {
			db.Create(&store.Outbound{ID: tag, NodeID: "n", Tag: tag, Type: model.OutBalancer, Settings: mustJSON(model.OutboundSettings{Members: []string{"direct"}})})
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, pair := range [][2]string{{"a", "b"}, {"b", "a"}} {
			wg.Add(1)
			go func(i int, pair [2]string) {
				defer wg.Done()
				<-start
				errs[i] = ss[i].storeOutbound("n", &store.Outbound{ID: pair[0]}, &outboundReq{Tag: pair[0], Type: model.OutBalancer, Settings: mustJSON(model.OutboundSettings{Members: []string{"direct"}, Fallback: pair[1]})})
			}(i, pair)
		}
		close(start)
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("exactly one edit must commit: %v", errs)
		}
		var rows []store.Outbound
		db.Find(&rows)
		var obs []model.Outbound
		for _, o := range rows {
			obs = append(obs, model.Outbound{Tag: o.Tag, Type: o.Type, Settings: o.Settings})
		}
		if err := model.ValidateOutboundGraph(obs); err != nil {
			t.Fatalf("concurrent edits corrupted graph: %v", err)
		}
	})
}

// Xray selects balancer members by tag prefix: a member that prefixes
// another outbound's tag is refused, in either order of creation.
func TestBalancerXrayPrefixClash(t *testing.T) {
	s, _, admin := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n", Name: "n"})
	db.Create(&store.Core{ID: "xray", NodeID: "n", Type: model.CoreXray, Version: "26.3.27"})
	db.Create(&store.Outbound{ID: "a", NodeID: "n", Tag: "hk", Type: model.OutSOCKS})
	db.Create(&store.Outbound{ID: "b", NodeID: "n", Tag: "hk2", Type: model.OutSOCKS})
	base := "/nodes/n/outbounds"
	group := func(members ...string) map[string]any {
		return map[string]any{"tag": "lb", "type": "balancer", "settings": model.OutboundSettings{Members: members, Strategy: model.LBRoundRobin}}
	}
	if code, out := admin.do("POST", base, group("hk")); code != 400 {
		t.Fatalf("member prefixing another tag: %d %v", code, out)
	}
	if code, out := admin.do("POST", base, group("hk", "hk2")); code != 200 {
		t.Fatalf("both members: %d %v", code, out)
	}
	// A new outbound the group would silently take in.
	if code, out := admin.do("POST", base, map[string]any{"tag": "hk-backup", "type": "socks"}); code != 400 {
		t.Fatalf("new tag joining a group by prefix: %d %v", code, out)
	}
	if code, out := admin.do("POST", base, map[string]any{"tag": "jp", "type": "socks"}); code != 200 {
		t.Fatalf("unrelated outbound: %d %v", code, out)
	}
}
