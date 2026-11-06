package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestProxyChainRejectsConflictingEntry(t *testing.T) {
	for _, problem := range []string{"duplicate", "bound-outbound", "disabled-entry", "disabled-landing"} {
		t.Run(problem, func(t *testing.T) {
			s, c, id, _, _ := proxyChangeFixture(t)
			var ch store.Chain
			s.st.DB.First(&ch, "id = ?", id)
			method, path := "PUT", "/chains/"+id
			switch problem {
			case "duplicate":
				method, path = "POST", "/chains"
				ch.Name = "competing-chain"
			case "bound-outbound":
				s.st.DB.Model(&store.Inbound{}).Where("id = ?", "proxy-in").Update("outbound_tag", "direct")
			case "disabled-entry":
				s.st.DB.Model(&store.Inbound{}).Where("id = ?", "proxy-in").Update("enabled", false)
			case "disabled-landing":
				s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("enabled", false)
			}
			if code, out := c.do(method, path, chainReq{Name: ch.Name, Mode: ch.Mode, Hops: ch.Hops.V}); code != 400 {
				t.Fatal("unusable or competing route accepted", problem, code, out)
			}
		})
	}
}

func TestProxyChainSubscriptionUsesEntryAndChainName(t *testing.T) {
	s, c, id, _, _ := proxyChangeFixture(t)
	u := relayTestUser(t, s)
	s.st.DB.Where("user_id = ?", u.ID).Delete(&store.UserAccess{})
	s.st.DB.Create(&store.UserAccess{UserID: u.ID, Kind: "inbound", TargetID: "proxy-in"})
	var ch store.Chain
	s.st.DB.First(&ch, "id = ?", id)
	ch.Name = "hk-to-tw-unique"
	if code, out := c.do("PUT", "/chains/"+id, chainReq{Name: ch.Name, Mode: ch.Mode, Hops: ch.Hops.V}); code != 200 {
		t.Fatal("editing the chain's own entry must remain possible", code, out)
	}
	nodes, err := s.subNodes(&u)
	if err != nil || len(nodes) != 1 || !strings.Contains(nodes[0].Name, ch.Name) || nodes[0].Server != "relay.example.com" || nodes[0].Port != 12345 {
		t.Fatal("chain must be findable by name and connect to the authorized entry", nodes, err)
	}
}

func TestChainProbeRejectsMalformedLatency(t *testing.T) {
	s, _, id, _, _ := proxyChangeFixture(t)
	defer fakeProber(t, s, "relay", map[string]string{"land.example.com:443": "not-a-latency"})()
	defer fakeProber(t, s, "land", map[string]string{"": "-1"})()
	var ch store.Chain
	s.st.DB.First(&ch, "id = ?", id)
	for _, p := range s.runChainProbe(&ch) {
		if p.OK || p.Error == "" {
			t.Fatal("malformed or negative latency reported as successful", p)
		}
	}
}

func TestProxyChainDeletionRemovesPublicationWithoutDirectFallback(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit-endpoint", true: "legacy-chain"}[legacy], func(t *testing.T) {
			s, c, id, _, _ := proxyChangeFixture(t)
			u := relayTestUser(t, s)
			s.st.DB.Where("user_id = ?", u.ID).Delete(&store.UserAccess{})
			s.st.DB.Create(&store.UserAccess{UserID: u.ID, Kind: "inbound", TargetID: "proxy-in"})
			other := store.User{ID: "unrelated-user", Username: "unrelated-user", UUID: "22222222-2222-4222-8222-222222222222", Password: "other-password", SubToken: "unrelated-user-token", Status: "active"}
			if err := s.st.DB.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			s.st.DB.Create(&store.UserAccess{UserID: other.ID, Kind: "inbound", TargetID: "in"})
			if legacy {
				s.st.DB.Where("relay_chain_id = ?", id).Delete(&store.Endpoint{})
				s.st.DB.Model(&store.Inbound{}).Where("id = ?", "proxy-in").Update("relay_only", false)
			}
			if nodes, err := s.subNodes(&u); err != nil || len(nodes) != 1 {
				t.Fatal("active path must be published", nodes, err)
			}
			if code, out := c.do("DELETE", "/chains/"+id, nil); code != 204 {
				t.Fatal(code, out)
			}
			if nodes, err := s.subNodes(&u); err != nil || len(nodes) != 0 {
				t.Fatal("deleted path reappeared as a direct subscription node", nodes, err)
			}
			if nodes, err := s.subNodes(&other); err != nil || len(nodes) != 1 || nodes[0].Server != "land.example.com" {
				t.Fatal("removing one path affected unrelated subscription grants", nodes, err)
			}
			if code, out := c.do("GET", "/users/"+u.ID+"/links", nil); code != 200 || out["links"] != nil {
				t.Fatal("deleted path remains in share links", code, out)
			}
			var in store.Inbound
			if err := s.st.DB.First(&in, "id = ?", "proxy-in").Error; err != nil || !in.Enabled || !in.RelayOnly {
				t.Fatal("keep the inbound without restoring direct publication", err)
			}
		})
	}
}

func TestProxyChainEndpointWithdrawalDoesNotRepublish(t *testing.T) {
	s, c, id, _, _ := proxyChangeFixture(t)
	u := relayTestUser(t, s)
	s.st.DB.Where("user_id = ?", u.ID).Delete(&store.UserAccess{})
	s.st.DB.Create(&store.UserAccess{UserID: u.ID, Kind: "inbound", TargetID: "proxy-in"})
	var ep store.Endpoint
	if err := s.st.DB.First(&ep, "relay_chain_id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if code, out := c.do("DELETE", "/inbounds/proxy-in/endpoints/"+ep.ID, nil); code != 204 {
		t.Fatal(code, out)
	}
	if nodes, err := s.subNodes(&u); err != nil || len(nodes) != 0 {
		t.Fatal("withdrawn endpoint was resurrected", nodes, err)
	}
	if code, rows := c.doList("GET", "/chains"); code != 200 || len(rows) != 1 || rows[0].(map[string]any)["published"] != false {
		t.Fatal("withdrawn entry still shown as published", code, rows)
	}
}

func TestProxyChainRejectsPQOnlyRealityForSingbox(t *testing.T) {
	s, c, id, _, _ := proxyChangeFixture(t)
	s.st.DB.Model(&store.Core{}).Where("id = ?", "relay-core").Updates(map[string]any{"type": model.CoreSingbox, "version": "1.13.5"})
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "proxy-in").Update("core", model.CoreSingbox)
	security := coretest.Reality(t)
	security.RealityPrivateKey = s.st.Box.Seal(security.RealityPrivateKey)
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("security", store.NewJSON(security))
	s.st.DB.Model(&store.Core{}).Where("id = ?", "xray").Updates(map[string]any{"version": "26.3.27", "running_version": model.XrayPQRealityVersion})
	var ch store.Chain
	s.st.DB.First(&ch, "id = ?", id)
	if code, out := c.do("PUT", "/chains/"+id, chainReq{Name: ch.Name, Mode: ch.Mode, Hops: ch.Hops.V}); code != 400 || !strings.Contains(out["error"].(string), "X25519MLKEM768") {
		t.Fatal("incompatible REALITY next hop accepted", code, out)
	}
	if _, err := s.BuildSpec("relay"); err == nil || !strings.Contains(err.Error(), "X25519MLKEM768") {
		t.Fatal("existing node_ref rendered an incompatible client", err)
	}
}
