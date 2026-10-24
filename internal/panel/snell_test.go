package panel

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/core/singbox"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func snellPanelFixture(t *testing.T) (*Server, *apiClient, string) {
	t.Helper()
	s, _, c := newAdminClient(t)
	if err := s.st.DB.Create(&store.Node{ID: "snell-node", Name: "Snell node", Address: "127.0.0.1", TrafficRate: 2, AgentFeatures: store.NewJSON([]string{model.SnellUsersFeature})}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.Core{ID: "snell-core", NodeID: "snell-node", Type: model.CoreSingbox, Version: "1.14.4"}).Error; err != nil {
		t.Fatal(err)
	}
	code, out := c.do("POST", "/inbounds", map[string]any{"nodeId": "snell-node", "tag": "snell-entry", "core": "singbox", "listen": "127.0.0.1", "port": coretest.FreePort(t), "protocol": map[string]any{"type": "snell"}, "transport": map[string]any{"type": "tcp"}, "security": map[string]any{"type": "none"}})
	if code != 201 {
		t.Fatal(code, out)
	}
	id := out["inbound"].(map[string]any)["id"].(string)
	for i, u := range coretest.Users() {
		row := store.User{ID: u.ID, Username: u.ID, UUID: u.UUID, SubToken: u.ID, Password: u.Password, SSSeed: u.SSSeed, Status: "active"}
		if i == 0 {
			row.TrafficLimit = 98303
		}
		if err := s.st.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.st.DB.Create(&store.UserAccess{UserID: u.ID, Kind: "inbound", TargetID: id}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return s, c, id
}

func TestSnellPanelLifecycleSecretsAndCompatibility(t *testing.T) {
	s, c, id := snellPanelFixture(t)
	var row store.Inbound
	s.st.DB.First(&row, "id = ?", id)
	original := s.toModelInbound(&row).Protocol.ServerKey
	if original == "" || row.Protocol.V.ServerKey == original {
		t.Fatal("PSK not encrypted at rest")
	}
	code, out := c.do("GET", "/inbounds/"+id, nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), original) || out["protocol"].(map[string]any)["serverKey"] != nil {
		t.Fatal("PSK exposed to editor")
	}
	body := map[string]any{"nodeId": "snell-node", "tag": "renamed", "core": "singbox", "listen": "127.0.0.1", "port": row.Port, "protocol": map[string]any{"type": "snell", "snellVersion": 6}, "transport": map[string]any{"type": "tcp"}, "security": map[string]any{"type": "none"}}
	if code, out = c.do("PUT", "/inbounds/"+id, body); code != 200 {
		t.Fatal(code, out)
	}
	row = store.Inbound{}
	s.st.DB.First(&row, "id = ?", id)
	if s.toModelInbound(&row).Protocol.ServerKey != original {
		t.Fatal("editing regenerated PSK")
	}
	if _, code, err := s.setCoreVersion("snell-node", model.CoreSingbox, "1.13.5", false); err == nil || code != 409 {
		t.Fatal("downgrade accepted", code, err)
	}
	var node store.Node
	s.st.DB.First(&node, "id = ?", "snell-node")
	m := s.toModelInbound(&row)
	node.AgentFeatures = store.NewJSON([]string{})
	if s.assignCore(&node, &m) == nil {
		t.Fatal("old Agent accepted")
	}
	node.AgentFeatures = store.NewJSON([]string{model.SnellUsersFeature})
	s.st.DB.Model(&store.Core{}).Where("id = ?", "snell-core").Update("no_user_traffic", true)
	if s.assignCore(&node, &m) == nil {
		t.Fatal("unmetered core accepted")
	}
	s.st.DB.Model(&store.Core{}).Where("id = ?", "snell-core").Update("no_user_traffic", false)
	assertUsers := func(want int) {
		t.Helper()
		spec, e := s.BuildSpec("snell-node")
		if e != nil {
			t.Fatal(e)
		}
		if len(spec.Inbounds) != 1 || len(spec.Inbounds[0].Users) != want {
			t.Fatalf("wrong authorised users %v", spec.Inbounds)
		}
	}
	assertUsers(2)
	s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Update("status", "disabled")
	assertUsers(1)
	expired := time.Now().Add(-time.Hour)
	s.st.DB.Model(&store.User{}).Where("id = ?", "u2").Update("expire_at", expired)
	s.enforceLimits()
	assertUsers(0)
	spec, _ := s.BuildSpec("snell-node")
	b, e := singbox.New(core.Options{Version: "1.14.4"}).Render(spec)
	if e != nil || strings.Contains(string(b), `"type": "snell"`) {
		t.Fatal("zero authorised users left shared-PSK listener", e)
	}
}

func TestSnellAgentFetchNegotiation(t *testing.T) {
	s, _, _ := snellPanelFixture(t)
	s.st.DB.Model(&store.Node{}).Where("id = ?", "snell-node").Updates(map[string]any{"cert_serial": "1", "desired_rev": 7})
	spec, _ := s.BuildSpec("snell-node")
	b, _ := json.Marshal(spec)
	if e := s.st.DB.Create(&store.ConfigRevision{NodeID: "snell-node", Rev: 7, Spec: s.st.Box.Seal(string(b))}).Error; e != nil {
		t.Fatal(e)
	}
	ctx := context.WithValue(context.Background(), peerCertKey, &x509.Certificate{Subject: pkix.Name{CommonName: "snell-node"}, SerialNumber: big.NewInt(1)})
	svc := &agentService{s: s}
	req := connect.NewRequest(&polarisv1.GetSpecRequest{Rev: 7})
	if _, e := svc.GetSpec(ctx, req); connect.CodeOf(e) != connect.CodeFailedPrecondition {
		t.Fatal("old agent fetched Snell credentials", e)
	}
	req.Header().Set("X-Polaris-Agent-Features", model.SnellUsersFeature)
	if _, e := svc.GetSpec(ctx, req); e != nil {
		t.Fatal(e)
	}
}
