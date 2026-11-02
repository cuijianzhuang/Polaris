package panel

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"connectrpc.com/connect"
	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func remoteTestToken(secret string) string {
	b, _ := json.Marshal(map[string]any{"a": strings.Repeat("a", 32), "t": testTunnelUUID, "s": []byte(secret)})
	return base64.StdEncoding.EncodeToString(b)
}
func TestRemoteTunnelInstallWithoutInboundAndCredentialLifecycle(t *testing.T) {
	s, a := tunnelFixture(t) // No inbound: the connector must install and start independently.
	features := []string{model.ManagedTunnelFeature, model.RemoteTunnelFeature}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Updates(map[string]any{"agent_features": store.NewJSON([]string{model.ManagedTunnelFeature}), "cert_serial": "1"})
	token := remoteTestToken(strings.Repeat("remote-private-fixture", 2))
	body := map[string]any{"enabled": true, "nodeId": "n1", "credentialMode": "remote", "token": token}
	if code, _ := a.do("PUT", "/cdn-profiles/cf/tunnel-agent", body); code != 400 {
		t.Fatal("JSON-only Agent accepted remote token", code)
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Update("agent_features", store.NewJSON(features))
	if code, out := a.do("PUT", "/cdn-profiles/cf/tunnel-agent", body); code != 200 {
		t.Fatal(code, out)
	}
	var profile store.CDNProfile
	s.st.DB.First(&profile, "id = ?", "cf")
	if profile.TunnelCredentialMode != "remote" || profile.TunnelCredential == token {
		t.Fatal("token not sealed or missing mode")
	}
	rev, _, err := s.Publish("n1", "remote install", true)
	if err != nil {
		t.Fatal(err)
	}
	var r store.ConfigRevision
	s.st.DB.First(&r, "node_id = ? AND rev = ?", "n1", rev)
	raw, err := s.st.Box.Open(r.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, token) || strings.Contains(raw, "tunnelCredentials") {
		t.Fatal("token in revision history")
	}
	var spec model.NodeSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Tunnels) != 1 || spec.Tunnels[0].CredentialMode != "remote" || len(spec.Tunnels[0].Routes) != 0 || !spec.HasCore(model.CoreCloudflared) {
		t.Fatal("missing remote connector without inbound", spec.Tunnels)
	}
	svc := &agentService{s: s}
	req := connect.NewRequest(&polarisv1.GetSpecRequest{Rev: rev})
	req.Header().Set("X-Polaris-Agent-Features", model.ManagedTunnelFeature)
	if _, err := svc.GetSpec(tunnelPeer("n1"), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("old Agent received token", err)
	}
	req.Header().Set("X-Polaris-Agent-Features", strings.Join(features, ","))
	res, err := svc.GetSpec(tunnelPeer("n1"), req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Credentials map[string]string `json:"tunnelCredentials"`
	}
	if err := json.Unmarshal(res.Msg.SpecJson, &wire); err != nil || wire.Credentials["cf"] != token {
		t.Fatal("remote credential not delivered as opaque string", err)
	}
	if _, err := s.deliverTunnelCredentials("other-node", spec.Tunnels); err == nil {
		t.Fatal("wrong node authorized")
	}
	for _, path := range []string{"/cdn-profiles", "/cdn-profiles/cf/tunnel-agent"} {
		code, out := a.doList("GET", path)
		b, _ := json.Marshal(out)
		if code != 200 || strings.Contains(string(b), token) || strings.Contains(string(b), profile.TunnelCredential) {
			t.Fatal("read API failed or leaked", path, code)
		}
	}
	if code, _ := a.do("PUT", "/cdn-profiles/cf/tunnel-agent", map[string]any{"enabled": true, "nodeId": "n1", "credentialMode": "local", "credentials": managedCredential(testTunnelUUID, strings.Repeat("s", 32))}); code != 400 {
		t.Fatal("mode changed without cleanup")
	}
	body["token"] = remoteTestToken(strings.Repeat("rotated-private-fixture", 2))
	if code, out := a.do("PUT", "/cdn-profiles/cf/tunnel-agent", body); code != 200 {
		t.Fatal(code, out)
	}
	if _, err := s.deliverTunnelCredentials("n1", spec.Tunnels); err == nil {
		t.Fatal("old revision fetched rotated token")
	}
	rev, _, err = s.Publish("n1", "remote rotation", true)
	if err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Update("applied_rev", rev)
	if code, out := a.do("PUT", "/cdn-profiles/cf/tunnel-agent", map[string]any{"enabled": false}); code != 200 || out["cleanupConfirmed"] != false {
		t.Fatal("disable prematurely acknowledged", code, out)
	}
	req.Msg.Rev = rev
	if _, err := svc.GetSpec(tunnelPeer("n1"), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("disabled credential delivered", err)
	}
	rev, _, err = s.Publish("n1", "stop remote", true)
	if err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Update("applied_rev", rev)
	if code, out := a.do("GET", "/cdn-profiles/cf/tunnel-agent", nil); code != 200 || out["cleanupConfirmed"] != true {
		t.Fatal("missing cleanup acknowledgment", code, out)
	}
}

func TestRemoteTunnelRejectsLegacyCore(t *testing.T) {
	s, a := tunnelFixture(t)
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Update("agent_features", store.NewJSON([]string{model.ManagedTunnelFeature, model.RemoteTunnelFeature}))
	if err := s.st.DB.Create(&store.Core{ID: store.NewID(), NodeID: "n1", Type: model.CoreCloudflared, Version: "2025.3.0"}).Error; err != nil {
		t.Fatal(err)
	}
	if code, out := a.do("PUT", "/cdn-profiles/cf/tunnel-agent", map[string]any{"enabled": true, "nodeId": "n1", "credentialMode": "remote", "token": remoteTestToken(strings.Repeat("s", 32))}); code != 400 || !strings.Contains(out["error"].(string), "2025.4.0") {
		t.Fatal("legacy core accepted", code, out)
	}
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", "cf")
	if p.TunnelManaged || p.TunnelCredential != "" {
		t.Fatal("failed install persisted credentials")
	}
}
