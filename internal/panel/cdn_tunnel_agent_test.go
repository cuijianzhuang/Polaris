package panel

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"connectrpc.com/connect"
	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func managedCredential(id, secret string) string {
	b, _ := json.Marshal(map[string]string{"AccountTag": strings.Repeat("a", 32), "TunnelID": id, "TunnelSecret": base64.StdEncoding.EncodeToString([]byte(secret))})
	return string(b)
}
func managedPanelFixture(t *testing.T) (*Server, *apiClient, store.Inbound) {
	t.Helper()
	s, a := tunnelFixture(t)
	in := addTunnelInbound(t, s, a)
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Updates(map[string]any{"agent_features": store.NewJSON([]string{model.ManagedTunnelFeature}), "cert_serial": "1"})
	return s, a, in
}
func enableManaged(t *testing.T, a *apiClient, credential string) {
	t.Helper()
	if code, out := a.do("PUT", "/cdn-profiles/cf/tunnel-agent", map[string]any{"enabled": true, "nodeId": "n1", "credentials": credential}); code != 200 {
		t.Fatal(code, out)
	}
}
func tunnelPeer(node string) context.Context {
	return context.WithValue(context.Background(), peerCertKey, &x509.Certificate{Subject: pkix.Name{CommonName: node}, SerialNumber: big.NewInt(1)})
}

func TestManagedTunnelCredentialDeliveryAndRotation(t *testing.T) {
	s, a, _ := managedPanelFixture(t)
	secret := strings.Repeat("private-fixture", 3)
	credential := managedCredential(testTunnelUUID, secret)
	enableManaged(t, a, credential)
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", "cf")
	if p.TunnelCredential == "" || strings.Contains(p.TunnelCredential, secret) || strings.Contains(p.TunnelCredential, "TunnelSecret") {
		t.Fatal("credential was not sealed")
	}
	rev, _, err := s.Publish("n1", "managed test", true)
	if err != nil {
		t.Fatal(err)
	}
	var r store.ConfigRevision
	s.st.DB.First(&r, "node_id = ? AND rev = ?", "n1", rev)
	raw, err := s.st.Box.Open(r.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "TunnelSecret") || strings.Contains(raw, base64.StdEncoding.EncodeToString([]byte(secret))) || strings.Contains(raw, "tunnelCredentials") {
		t.Fatal("plaintext credentials entered revision history")
	}
	var saved model.NodeSpec
	json.Unmarshal([]byte(raw), &saved)
	if len(saved.Tunnels) != 1 || saved.Tunnels[0].ProfileID != "cf" || saved.Tunnels[0].CredentialVersion != p.TunnelCredentialVersion {
		t.Fatal("missing public tunnel metadata", saved.Tunnels)
	}
	service := &agentService{s: s}
	req := connect.NewRequest(&polarisv1.GetSpecRequest{Rev: rev})
	if _, err := service.GetSpec(tunnelPeer("n1"), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("old Agent received managed spec", err)
	}
	req.Header().Set("X-Polaris-Agent-Features", model.ManagedTunnelFeature)
	res, err := service.GetSpec(tunnelPeer("n1"), req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Tunnels     []model.TunnelSpec         `json:"tunnels"`
		Credentials map[string]json.RawMessage `json:"tunnelCredentials"`
	}
	json.Unmarshal(res.Msg.SpecJson, &wire)
	if len(wire.Credentials) != 1 || !strings.Contains(string(wire.Credentials["cf"]), "TunnelSecret") {
		t.Fatal("authorized Agent did not receive credential")
	}
	// A stolen old revision must not authorize a different node.
	s.st.DB.Create(&store.Node{ID: "n2", Name: "other", CertSerial: "1"})
	s.st.DB.Create(&store.ConfigRevision{NodeID: "n2", Rev: rev, Spec: s.st.Box.Seal(raw)})
	if _, err := service.GetSpec(tunnelPeer("n2"), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("wrong node fetched credential", err)
	}
	oldVersion := p.TunnelCredentialVersion
	enableManaged(t, a, managedCredential(testTunnelUUID, strings.Repeat("rotated-fixture", 3)))
	s.st.DB.First(&p, "id = ?", "cf")
	if p.TunnelCredentialVersion == oldVersion {
		t.Fatal("credential version did not rotate")
	}
	if _, err := service.GetSpec(tunnelPeer("n1"), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("old revision fetched rotated credential", err)
	}
	code, view := a.doList("GET", "/cdn-profiles")
	if code != 200 {
		t.Fatal(code, view)
	}
	// Read APIs never echo credentials. apiClient represents array responses in data.
	b, _ := json.Marshal(view)
	if strings.Contains(string(b), "TunnelSecret") {
		t.Fatal("read API exposed credentials")
	}
	if code, out := a.do("PUT", "/cdn-profiles/cf/tunnel-agent", map[string]any{"enabled": false}); code != 200 {
		t.Fatal(code, out)
	}
	s.st.DB.First(&p, "id = ?", "cf")
	if p.TunnelCredential != "" || p.TunnelManaged || p.TunnelNodeID != "n1" {
		t.Fatal("disable failed to erase panel credential or track cleanup")
	}
	if _, err := service.GetSpec(tunnelPeer("n1"), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("disabled mode delivered credential", err)
	}
}

func TestManagedTunnelValidationAndCleanupState(t *testing.T) {
	s, a, _ := managedPanelFixture(t)
	for _, credential := range []string{"remote-token", managedCredential("22345678-abcd-1234-abcd-123456789abc", strings.Repeat("x", 32)), strings.Repeat("secret", 3000)} {
		if code, out := a.do("PUT", "/cdn-profiles/cf/tunnel-agent", map[string]any{"enabled": true, "nodeId": "n1", "credentials": credential}); code != 400 {
			t.Fatal("invalid credential accepted", code, out)
		}
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Update("agent_features", store.NewJSON([]string{}))
	credential := managedCredential(testTunnelUUID, strings.Repeat("s", 32))
	if code, out := a.do("PUT", "/cdn-profiles/cf/tunnel-agent", map[string]any{"enabled": true, "nodeId": "n1", "credentials": credential}); code != 400 {
		t.Fatal("unsupported Agent accepted", code, out)
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Update("agent_features", store.NewJSON([]string{model.ManagedTunnelFeature}))
	enableManaged(t, a, credential)
	rev, _, err := s.Publish("n1", "active", true)
	if err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Update("applied_rev", rev)
	if code, out := a.do("PUT", "/cdn-profiles/cf/tunnel-agent", map[string]any{"enabled": false}); code != 200 || out["cleanupConfirmed"] != false {
		t.Fatal("premature cleanup confirmation", code, out)
	}
	if code, out := a.do("PUT", "/cdn-profiles/cf", map[string]any{"name": "cf-main", "originDomain": "cdn.example.com", "originMode": "direct"}); code != 400 {
		t.Fatal("changed origin before cleanup", code, out)
	}
	rev, _, err = s.Publish("n1", "stop", true)
	if err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Update("applied_rev", rev)
	if code, out := a.do("GET", "/cdn-profiles/cf/tunnel-agent", nil); code != 200 || out["cleanupConfirmed"] != true {
		t.Fatal("cleanup acknowledgment not recognized", code, out)
	}
	_, token, err := s.issueAPIToken("probe", "", []string{ScopeCDNIPs}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	probe := &apiClient{t: t, base: a.base, tok: token}
	if code, _ := probe.do("PUT", "/cdn-profiles/cf/tunnel-agent", map[string]any{"enabled": true, "nodeId": "n1", "credentials": credential}); code != 403 {
		t.Fatal("probe token changed tunnel management", code)
	}
}
