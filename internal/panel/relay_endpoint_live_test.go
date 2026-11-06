package panel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/core/gost"
	"github.com/cuijianzhuang/polaris/internal/core/singbox"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"golang.org/x/net/proxy"
)

// The client consumes the panel's actual subscription, trusts the test CA,
// and verifies the landing name even though it dials a different relay host.
func TestLiveAnyTLSRelaySubscription(t *testing.T)  { liveRelaySubscription(t, false) }
func TestLiveRealityRelaySubscription(t *testing.T) { liveRelaySubscription(t, true) }

func liveRelaySubscription(t *testing.T, reality bool) {
	bin, gbin := os.Getenv("POLARIS_SINGBOX_BIN"), os.Getenv("POLARIS_GOST_BIN")
	if bin == "" || gbin == "" {
		t.Skip("POLARIS_SINGBOX_BIN and POLARIS_GOST_BIN required")
	}
	s, c := relayFixture(t)
	u := relayTestUser(t, s)
	tls := coretest.TLS(t, "localhost")
	cert := store.Certificate{ID: "relay-cert", Domain: "localhost", Provider: "manual", Cert: tls.CertPEM, Key: s.st.Box.Seal(tls.KeyPEM)}
	if err := s.st.DB.Create(&cert).Error; err != nil {
		t.Fatal(err)
	}
	protocol := model.ProtocolSpec{Type: model.ProtoAnyTLS}
	security := model.SecuritySpec{Type: model.SecTLS, CertificateID: cert.ID}
	if reality {
		dest := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		dest.EnableHTTP2 = true
		dest.StartTLS()
		defer dest.Close()
		protocol = model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"}
		security = coretest.Reality(t)
		security.RealityDest = strings.TrimPrefix(dest.URL, "https://")
		security.RealityPrivateKey = s.st.Box.Seal(security.RealityPrivateKey)
		// Simulate an older import retaining another key pair's public half.
		security.RealityPublicKey = coretest.Reality(t).RealityPublicKey
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "land").Update("address", "localhost")
	s.st.DB.Model(&store.Node{}).Where("id = ?", "relay").Update("address", "127.0.0.1")
	s.st.DB.Model(&store.Core{}).Where("id = ?", "xray").Updates(map[string]any{"type": model.CoreSingbox, "version": "1.13.5"})
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Updates(map[string]any{
		"core": model.CoreSingbox, "listen": "::", "port": coretest.FreePort(t), "relay_only": true,
		"protocol": store.NewJSON(protocol), "security": store.NewJSON(security),
	})
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	isolateLocalRelayPorts(t, s, id)
	land, err := s.BuildSpec("land")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a := singbox.New(core.Options{Version: "1.13.5", BinPath: bin, WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))})
	raw, err := a.Render(land)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Validate(raw); err != nil {
		t.Fatal(err)
	}
	if err = core.AtomicWrite(filepath.Join(dir, "singbox.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	relay, err := s.BuildSpec("relay")
	if err != nil || len(relay.Forwards) != 1 {
		t.Fatal(relay, err)
	}
	// Use GOST's native raw TCP forwarder for the generated L4 target.
	relay.Forwards[0].Engine = model.EngineGost
	gd := t.TempDir()
	ga := gost.New(core.Options{Version: "3.2.6", BinPath: gbin, WorkDir: gd, APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))})
	raw, err = ga.Render(relay)
	if err != nil {
		t.Fatal(err)
	}
	if err = core.AtomicWrite(filepath.Join(gd, "gost.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = ga.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer ga.Stop(context.Background())
	nodes, err := s.subNodes(&u)
	if err != nil || len(nodes) != 1 {
		t.Fatal(nodes, err)
	}
	relaySubscriptionTransfer(t, bin, nodes[0], tls.CertPEM)
}

func relaySubscriptionTransfer(t *testing.T, bin string, node sub.Node, certPEM string) {
	t.Helper()
	dir := t.TempDir()
	// Decode the exported URI too, so the test covers a normal client import.
	parsed, err := sub.ParseLink(sub.ShareLink(node))
	if err != nil {
		t.Fatal(parsed, err)
	}
	out := sub.SingboxOutbound(parsed)
	ca := filepath.Join(dir, "ca.pem")
	if err = os.WriteFile(ca, []byte(certPEM), 0600); err != nil {
		t.Fatal(err)
	}
	if node.TLS.Type == model.SecTLS {
		out["tls"].(map[string]any)["certificate_path"] = ca
	}
	port := coretest.FreePort(t)
	raw, _ := json.Marshal(map[string]any{"log": map[string]any{"level": "error"}, "inbounds": []any{map[string]any{"type": "socks", "listen": "127.0.0.1", "listen_port": port}}, "outbounds": []any{out}})
	clientPath := filepath.Join(dir, "client.json")
	if err = os.WriteFile(clientPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "run", "-c", clientPath)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	ready := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		conn, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if e == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("relay subscription client did not start")
	}
	dialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", port), nil, &net.Dialer{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	up, down := bytes.Repeat([]byte("upload"), 4096), bytes.Repeat([]byte("download"), 4096)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, e := io.ReadAll(r.Body)
		if e != nil || !bytes.Equal(body, up) {
			http.Error(w, "upload mismatch", http.StatusBadRequest)
			return
		}
		w.Write(down)
	}))
	defer backend.Close()
	roots := x509.NewCertPool()
	roots.AddCert(backend.Certificate())
	transport := &http.Transport{Dial: dialer.Dial, TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	res, err := client.Post(backend.URL, "application/octet-stream", bytes.NewReader(up))
	if err != nil {
		t.Fatal("generated relay subscription failed", node.Protocol, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != 200 || !bytes.Equal(body, down) {
		t.Fatal("relay upload/download failed", node.Protocol, res.StatusCode, err)
	}
}
