package panel

import (
	"connectrpc.com/connect"
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"math/big"
	"testing"
)

func TestSnellDedicatedOwnerStillRequiresGrant(t *testing.T) {
	s, _, id := snellPanelFixture(t)
	var in store.Inbound
	s.st.DB.First(&in, "id = ?", id)
	in.Protocol.V.SnellAuth = "psk"
	in.Protocol.V.SnellUserID = "u1"
	s.st.DB.Save(&in)
	spec, e := s.BuildSpec("snell-node")
	if e != nil || len(spec.Inbounds[0].Users) != 1 || spec.Inbounds[0].Users[0].ID != "u1" {
		t.Fatal("other grantees entered dedicated port", e)
	}
	m := s.toModelInbound(&in)
	if m.SnellClientPSK(model.User{ID: "u1", Password: "p"}) == m.SnellClientPSK(model.User{ID: "u2", Password: "p"}) {
		t.Fatal("different owners share PSK")
	}
	if m.SnellClientPSK(model.User{ID: "u1", Password: "p"}) == m.SnellClientPSK(model.User{ID: "u1", Password: "rotated"}) {
		t.Fatal("password reset did not rotate PSK")
	}
	s.st.DB.Where("user_id = ? AND kind = ? AND target_id = ?", "u1", "inbound", id).Delete(&store.UserAccess{})
	spec, e = s.BuildSpec("snell-node")
	if e != nil || len(spec.Inbounds[0].Users) != 0 {
		t.Fatal("owner selection bypassed grants", e)
	}
	var owner store.User
	s.st.DB.First(&owner, "id = ?", "u1")
	nodes, e := s.subNodes(&owner)
	if e != nil || len(nodes) != 0 {
		t.Fatal("owner without grant received subscription", e)
	}
	// A v1 Agent understands userkey but must never interpret the new dedicated mode.
	s.st.DB.Model(&store.Node{}).Where("id = ?", "snell-node").Updates(map[string]any{"cert_serial": "1", "desired_rev": 9})
	raw, _ := json.Marshal(spec)
	s.st.DB.Create(&store.ConfigRevision{NodeID: "snell-node", Rev: 9, Spec: s.st.Box.Seal(string(raw))})
	ctx := context.WithValue(context.Background(), peerCertKey, &x509.Certificate{Subject: pkix.Name{CommonName: "snell-node"}, SerialNumber: big.NewInt(1)})
	svc := &agentService{s: s}
	req := connect.NewRequest(&polarisv1.GetSpecRequest{Rev: 9})
	req.Header().Set("X-Polaris-Agent-Features", model.SnellUsersFeature)
	if _, e := svc.GetSpec(ctx, req); connect.CodeOf(e) != connect.CodeFailedPrecondition {
		t.Fatal("old Agent fetched dedicated PSK config", e)
	}
	req.Header().Set("X-Polaris-Agent-Features", model.SnellUsersFeature+","+model.SnellPSKFeature)
	if _, e := svc.GetSpec(ctx, req); e != nil {
		t.Fatal(e)
	}
}
