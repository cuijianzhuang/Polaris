package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestRoutesAndHealth(t *testing.T) {
	s := newTestServer(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "relay", Name: "relay", Address: "10.0.0.1"})
	db.Create(&store.Node{ID: "land1", Name: "land1", Address: "10.0.0.2"})
	db.Create(&store.Node{ID: "land2", Name: "land2", Address: "10.0.0.3"})
	db.Create(&store.Core{ID: "c1", NodeID: "relay", Type: model.CoreXray, Version: "26.3.27"})
	for _, l := range []string{"land1", "land2"} {
		db.Create(&store.Inbound{ID: "in-" + l, NodeID: l, Tag: "t", Port: 443, Core: model.CoreXray,
			Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoTrojan}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}),
			Security: store.NewJSON(model.SecuritySpec{Type: model.SecNone})})
		db.Create(&store.User{ID: "svc-" + l, Username: "svc-" + l, UUID: "u-" + l, Password: "p", Status: "active", SubToken: "tok-" + l, IsService: true})
		db.Create(&store.Outbound{ID: "o-" + l, NodeID: "relay", Tag: l, Type: model.OutNodeRef, NodeRefInboundID: "in-" + l, ServiceUserID: "svc-" + l})
	}
	db.Create(&store.Outbound{ID: "lb", NodeID: "relay", Tag: "lb", Type: model.OutBalancer,
		Settings: mustJSON(model.OutboundSettings{Members: []string{"land1", "land2"}, Strategy: model.LBRoundRobin})})
	db.Create(&store.Inbound{ID: "entry", NodeID: "relay", Tag: "entry", Port: 8443, OutboundTag: "lb", Core: model.CoreXray,
		Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoTrojan})})
	db.Create(&store.RoutingRule{ID: "g1", NodeID: model.GlobalNodeID, Priority: 5, OutboundTag: "block", Match: store.NewJSON(model.RouteMatch{GeoIP: []string{"private"}})})
	db.Create(&store.RoutingRule{ID: "g2", NodeID: model.GlobalNodeID, Priority: 6, OutboundTag: "warp", Match: store.NewJSON(model.RouteMatch{Geosite: []string{"netflix"}})})

	spec, err := s.BuildSpec("relay")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Routes) != 2 || spec.Routes[0].Outbound != "lb" || spec.Routes[0].Match.Inbound[0] != "entry" || spec.Routes[1].Outbound != "block" {
		t.Fatalf("routes: %+v", spec.Routes)
	}
	members := func() []string {
		spec, _ := s.BuildSpec("relay")
		for _, o := range spec.Outbounds {
			if o.Tag == "lb" {
				set, _ := o.ParseSettings()
				return set.Members
			}
		}
		return nil
	}
	if m := members(); len(m) != 2 {
		t.Fatalf("members %v", m)
	}
	s.health.set("land1", true)
	if m := members(); len(m) != 1 || m[0] != "land2" {
		t.Fatalf("unhealthy member not removed: %v", m)
	}
	s.health.set("land2", true)
	if m := members(); len(m) != 2 {
		t.Fatalf("all-down group must keep members: %v", m)
	}
	if err := s.validateBalancer("relay", "lb2", model.OutboundSettings{Members: []string{"nope"}}); err == nil {
		t.Fatal("unknown member accepted")
	}
	db.Create(&store.Core{ID: "c2", NodeID: "relay", Type: model.CoreSingbox, Version: "1.13.5"})
	if err := s.validateBalancer("relay", "lb2", model.OutboundSettings{Members: []string{"land1"}, Strategy: model.LBRoundRobin}); err == nil {
		t.Fatal("sing-box cannot round-robin")
	}
}
