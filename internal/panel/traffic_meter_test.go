package panel

import (
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func TestTrafficMeterHeartbeatAndLegacy(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "meter-node"})
	s.st.DB.Create(&store.Core{ID: "meter-core", NodeID: "meter-node", Type: model.CoreMihomo})
	now := time.Now().Unix()
	heartbeat := func(meter *polarisv1.TrafficMeter) store.Core {
		s.handleHeartbeat("meter-node", &polarisv1.Heartbeat{Cores: []*polarisv1.CoreState{{Type: "mihomo", Running: true, TrafficMeter: meter}}})
		var row store.Core
		if err := s.st.DB.First(&row, "id = ?", "meter-core").Error; err != nil {
			t.Fatal(err)
		}
		return row
	}
	row := heartbeat(&polarisv1.TrafficMeter{Mode: "connection_sampled", IntervalSeconds: 1, LastSuccess: now, LastAttempt: now})
	if row.TrafficMeter.V.LastSuccess == nil || row.TrafficMeter.V.LastSuccess.Unix() != now || row.TrafficMeter.V.Mode != "connection_sampled" {
		t.Fatal(row.TrafficMeter)
	}
	row = heartbeat(&polarisv1.TrafficMeter{Mode: "connection_sampled", IntervalSeconds: 1, LastSuccess: now, LastAttempt: now + 1, ConsecutiveFailures: 1})
	if row.TrafficMeter.V.ConsecutiveFailures != 1 {
		t.Fatal("failure was hidden by heartbeat throttling")
	}
	row = heartbeat(nil)
	if row.TrafficMeter.V.Mode != "" || row.TrafficMeter.V.LastSuccess != nil {
		t.Fatal("legacy agent inherited fabricated telemetry", row.TrafficMeter)
	}
}

func TestPortalMeterWarningOnlyForAuthorizedInbound(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "notice-node", Status: "online"})
	in := store.Inbound{ID: "notice-in", NodeID: "notice-node", Core: model.CoreXray}
	s.st.DB.Create(&in)
	s.st.DB.Create(&store.Core{ID: "unrelated-mihomo", NodeID: "notice-node", Type: model.CoreMihomo})
	node := sub.Node{Name: "test", Server: "example.test", Port: 443, Protocol: model.ProtoVLESS, UUID: "fixture", PanelInboundID: in.ID, PanelNodeID: in.NodeID}
	var info sub.Info
	if err := s.portalNodeDetails(&info, []sub.Node{node}); err != nil {
		t.Fatal(err)
	}
	if info.TrafficMeterNote != "" {
		t.Fatal("unrelated installed core changed precision notice")
	}
	s.st.DB.Model(&in).Update("core", model.CoreMihomo)
	if err := s.portalNodeDetails(&info, []sub.Node{node}); err != nil {
		t.Fatal(err)
	}
	if info.TrafficMeterNote == "" {
		t.Fatal("sampled user traffic had no precision notice")
	}
}
