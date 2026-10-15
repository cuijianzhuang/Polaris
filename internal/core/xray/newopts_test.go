package xray

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
)

// newOptsInbound is VLESS + Vision + REALITY with VLESS Encryption, an
// ML-DSA-65 signature, testseed and the REALITY limits.
func newOptsInbound(t *testing.T, port int, dest string) model.Inbound {
	dec, enc, err := pki.VLESSEncPair(pki.VLESSAuthMLKEM)
	if err != nil {
		t.Fatal(err)
	}
	seed, verify, err := pki.MLDSA65Pair()
	if err != nil {
		t.Fatal(err)
	}
	sec := coretest.Reality(t)
	sec.RealityDest, sec.RealityServerNames = dest, []string{"example.com"}
	sec.RealityMldsa65Seed, sec.RealityMldsa65Verify = seed, verify
	sec.RealityMinClientVer, sec.RealityMaxTimeDiff, sec.RealitySpiderX = "1.0.0", 60000, "/probe"
	sec.RealityLimitUpload = &model.LimitFallback{AfterBytes: 1 << 20, BytesPerSec: 1 << 18}
	in := coretest.Inbound(model.CoreXray, "vless-pq", port, model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision",
		Decryption: dec, Encryption: enc, TestSeed: []int{900, 500, 900, 256}}, model.TransportSpec{Type: model.TransTCP}, sec)
	in.Listen = "127.0.0.1"
	in.Sniffing = model.SniffSpec{Enabled: true, MetadataOnly: true, IPsExcluded: []string{"10.0.0.0/8"}, DomainsExcluded: []string{"domain:example.org"}}
	return in
}

func TestRenderNewOptions(t *testing.T) {
	in := newOptsInbound(t, 20443, "127.0.0.1:8443")
	px := coretest.Inbound(model.CoreXray, "vmess-pp", 20444, model.ProtocolSpec{Type: model.ProtoVMess},
		model.TransportSpec{Type: model.TransWS, Path: "/w", AcceptProxyProtocol: true}, model.SecuritySpec{})
	a := New(core.Options{Version: "26.10.10", WorkDir: t.TempDir(), BinPath: os.Getenv("POLARIS_XRAY_BIN")})
	if err := a.Capabilities("26.10.10").SupportsInbound(&in); err != nil {
		t.Fatal(err)
	}
	if err := a.Capabilities("25.8.3").SupportsInbound(&in); err == nil {
		t.Fatal("old xray must refuse VLESS Encryption")
	}
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in, px}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"decryption": "mlkem768x25519plus.native.600s.`, `"testseed": [`, `"mldsa65Seed"`,
		`"minClientVer": "1.0.0"`, `"maxTimeDiff": 60000`, `"limitFallbackUpload"`, `"metadataOnly": true`,
		`"ipsExcluded"`, `"acceptProxyProtocol": true`} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("missing %s", want)
		}
	}
	if os.Getenv("POLARIS_XRAY_BIN") != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("xray rejected config: %v\n%s", err, cfg)
		}
	}
}

// TestLiveVLESSEncryption connects through a client Xray rendered from the
// panel's outbound model to a server using the new options: VLESS
// Encryption and REALITY's ML-DSA-65 signature, separately and together.
func TestLiveVLESSEncryption(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_BIN")
	if bin == "" || !core.VersionAtLeast(xrayVersion(bin), "25.9.11") {
		t.Skip("POLARIS_XRAY_BIN not set or older than 25.9.11")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	for _, c := range []struct {
		name       string
		enc, mldsa bool
	}{{"encryption", true, false}, {"mldsa65", false, true}, {"both", true, true}} {
		t.Run(c.name, func(t *testing.T) { liveNewOpts(t, bin, c.enc, c.mldsa) })
	}
}

func liveNewOpts(t *testing.T, bin string, enc, mldsa bool) {
	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dest.EnableHTTP2 = true
	dest.Config.ErrorLog = log.New(io.Discard, "", 0) // REALITY probes the target on every handshake
	// ML-DSA-65 must fit its signature into a certificate record as large
	// as the target's: real targets send 3.5 KB+, so pad the test cert.
	var names []string
	for i := 0; i < 220; i++ {
		names = append(names, fmt.Sprintf("host-%03d.example.com", i))
	}
	certPEM, keyPEM, err := pki.SelfSigned(append([]string{"example.com"}, names...), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	dest.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	dest.StartTLS()
	defer dest.Close()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "through") }))
	defer web.Close()

	port := freePort(t)
	in := newOptsInbound(t, port, strings.TrimPrefix(dest.URL, "https://"))
	if !enc {
		in.Protocol.Decryption, in.Protocol.Encryption = "", ""
	}
	if !mldsa {
		in.Security.RealityMldsa65Seed, in.Security.RealityMldsa65Verify = "", ""
	}
	in.Extra = json.RawMessage(`{"streamSettings":{"realitySettings":{"show":true}}}`) // REALITY trace on failure
	srv := New(core.Options{Version: "26.10.10", BinPath: bin, WorkDir: t.TempDir(), APIListen: fmt.Sprintf("127.0.0.1:%d", freePort(t))})
	start(t, srv, &model.NodeSpec{Inbounds: []model.Inbound{in}})

	socks := freePort(t)
	land := model.Outbound{Tag: "land", Type: model.OutVLESS, Settings: coretest.Settings(model.OutboundSettings{
		Server: "127.0.0.1", Port: port, UUID: coretest.Users()[0].UUID, Flow: "xtls-rprx-vision", Encryption: in.Protocol.Encryption,
		Security: &model.ClientTLS{Type: model.SecReality, ServerName: "example.com", Fingerprint: "chrome",
			PublicKey: in.Security.RealityPublicKey, ShortID: in.Security.RealityShortIDs[0], SpiderX: "/probe",
			Mldsa65Verify: in.Security.RealityMldsa65Verify}})}
	cin := coretest.Inbound(model.CoreXray, "socks", socks, model.ProtocolSpec{Type: model.ProtoSOCKS}, model.TransportSpec{}, model.SecuritySpec{})
	cin.Listen = "127.0.0.1"
	cli := New(core.Options{Version: "26.10.10", BinPath: bin, WorkDir: t.TempDir(), APIListen: fmt.Sprintf("127.0.0.1:%d", freePort(t))})
	start(t, cli, &model.NodeSpec{Inbounds: []model.Inbound{cin}, Outbounds: []model.Outbound{land},
		Routes: []model.RouteRule{{Priority: 100, Outbound: "land"}}})

	u := coretest.Users()[0]
	var out []byte
	for i := 0; i < 30; i++ {
		out, err = exec.Command("curl", "-s", "-m", "5", "--socks5-hostname", fmt.Sprintf("127.0.0.1:%d", socks),
			"-U", u.ID+":"+u.Password, web.URL).CombinedOutput()
		if err == nil && string(out) == "through" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("no traffic: %v %q\nserver log:\n%s\nclient log:\n%s", err, out,
		strings.Join(srv.Logs(80), "\n"), strings.Join(cli.Logs(20), "\n"))
}

func start(t *testing.T, a *Adapter, spec *model.NodeSpec) {
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(cfg); err != nil {
		t.Fatalf("invalid config: %v\n%s", err, cfg)
	}
	if err := core.AtomicWrite(a.configPath(), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(ctx) })
}

func xrayVersion(bin string) string {
	out, _ := exec.Command(bin, "version").Output()
	f := strings.Fields(string(out))
	if len(f) > 1 {
		return f[1]
	}
	return ""
}

func TestRenderGeodata(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "xray")
	if src := os.Getenv("POLARIS_XRAY_BIN"); src != "" {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(bin, b, 0o755)
		for _, f := range []string{"geoip.dat", "geosite.dat"} {
			if d, err := os.ReadFile(filepath.Join(filepath.Dir(src), f)); err == nil {
				os.WriteFile(filepath.Join(dir, f), d, 0o644)
			}
		}
	}
	os.WriteFile(filepath.Join(dir, "geoip.dat"), mustRead(t, filepath.Join(dir, "geoip.dat")), 0o644)
	g := &model.GeodataSpec{Cron: "0 4 * * *", Assets: append(model.StandardGeoAssets[:1:1],
		model.GeoAsset{URL: "https://example.com/geosite_IR.dat", File: "geosite_IR.dat"})}
	a := New(core.Options{Version: "26.10.10", WorkDir: t.TempDir(), BinPath: bin})
	cfg, err := a.Render(&model.NodeSpec{Geodata: g})
	if err != nil {
		t.Fatal(err)
	}
	s := string(cfg)
	if !strings.Contains(s, `"geodata"`) || !strings.Contains(s, `"file": "geoip.dat"`) || strings.Contains(s, "geosite_IR.dat") {
		t.Fatalf("geodata block should list only present files:\n%s", s)
	}
	if old := New(core.Options{Version: "26.3.27", WorkDir: t.TempDir(), BinPath: bin}); old.geodata(g) != nil {
		t.Fatal("xray before 26.4.25 has no geodata")
	}
	if os.Getenv("POLARIS_XRAY_BIN") != "" && core.VersionAtLeast(xrayVersion(bin), "26.4.25") {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("xray rejected geodata: %v", err)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		return []byte("placeholder")
	}
	return b
}

func TestRenderTCPHTTPHeader(t *testing.T) {
	in := coretest.Inbound(model.CoreXray, "vmess-http", 20450, model.ProtocolSpec{Type: model.ProtoVMess},
		model.TransportSpec{Type: model.TransTCP, HeaderType: "http", Host: "a.example.com", Path: "/x", AcceptProxyProtocol: true}, model.SecuritySpec{})
	a := New(core.Options{Version: "26.3.27", WorkDir: t.TempDir(), BinPath: os.Getenv("POLARIS_XRAY_BIN")})
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(cfg); !strings.Contains(s, `"type": "http"`) || !strings.Contains(s, `"a.example.com"`) || !strings.Contains(s, `"acceptProxyProtocol": true`) {
		t.Fatalf("tcp http header not rendered:\n%s", s)
	}
	if os.Getenv("POLARIS_XRAY_BIN") != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatal(err)
		}
	}
}

// Xray 26.7.x defaults REALITY's minClientVer to 26.3.27, refusing sing-box
// and mihomo clients; the adapter opens it up unless one is configured.
func TestRealityMinClientVerDefault(t *testing.T) {
	sec := coretest.Reality(t)
	sec.RealityDest, sec.RealityServerNames = "127.0.0.1:8443", []string{"example.com"}
	in := coretest.Inbound(model.CoreXray, "r", 20445, model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"},
		model.TransportSpec{Type: model.TransTCP}, sec)
	for v, want := range map[string]bool{"26.3.27": false, "26.7.11": true, "26.7.28": true, "26.10.10": false} {
		a := New(core.Options{Version: v, WorkDir: t.TempDir()})
		cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(cfg), `"minClientVer": "0.0.0"`); got != want {
			t.Errorf("%s: minClientVer 0.0.0 = %v, want %v", v, got, want)
		}
	}
	if !hasStr(New(core.Options{}).Capabilities("26.10.10").Features, model.FeatureRealityPQOnly) ||
		hasStr(New(core.Options{}).Capabilities("26.7.28").Features, model.FeatureRealityPQOnly) {
		t.Error("reality-pq-only feature must start at " + model.XrayPQRealityVersion)
	}
}

func hasStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
