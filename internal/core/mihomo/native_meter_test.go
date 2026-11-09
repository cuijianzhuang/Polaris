package mihomo

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestNativeCumulativeShortConnectionsAndUDP(t *testing.T) {
	bin := os.Getenv("POLARIS_MIHOMO_COUNTER_BIN")
	if bin == "" {
		t.Skip("POLARIS_MIHOMO_COUNTER_BIN not set")
	}
	a := New(core.Options{Version: "1.19.15", WorkDir: t.TempDir(), BinPath: bin, APIListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(coretest.FreePort(t)))})
	in := coretest.Inbound(model.CoreMihomo, "meter", coretest.FreePort(t), model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{}, coretest.TLS(t, "meter.test"))
	in.Listen = "127.0.0.1"
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}})
	if err != nil {
		t.Fatal(err)
	}
	if err = core.AtomicWrite(a.configPath(), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	a.stopMeter() // No connection can be observed by the background sampler.
	for until := time.Now().Add(5 * time.Second); ; {
		if _, err = a.connectionSnapshot(t.Context()); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("controller not ready: %v %v", err, a.Logs(30))
		}
		time.Sleep(20 * time.Millisecond)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "closed-short-connection") }))
	defer backend.Close()
	for until := time.Now().Add(5 * time.Second); ; {
		conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(in.Port)), 100*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			break
		}
		if time.Now().After(until) {
			t.Fatalf("listener not ready: %v %v\n%s", dialErr, a.Logs(30), cfg)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Native mihomo requires TLS for VLESS. This loopback-only test bridge
	// validates the generated certificate and adds TLS to the raw fixture.
	bridge, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(in.Security.CertPEM))
	go func() {
		for {
			c, err := bridge.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				remote, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(in.Port)), &tls.Config{RootCAs: pool, ServerName: "meter.test", MinVersion: tls.VersionTLS12})
				if err != nil {
					return
				}
				defer remote.Close()
				go func() { io.Copy(remote, c); remote.Close() }()
				io.Copy(c, remote)
			}()
		}
	}()
	clientPort := bridge.Addr().(*net.TCPAddr).Port
	for _, u := range in.Users {
		for n := 0; n < 5; n++ {
			body, err := coretest.FetchVLESSUser(clientPort, backend.Listener.Addr().String(), u.UUID)
			if err != nil || body != "closed-short-connection" {
				t.Fatal(body, err)
			}
		}
	}
	for until := time.Now().Add(2 * time.Second); ; {
		connections, err := a.connections(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(connections) == 0 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("short connections did not close")
		}
		time.Sleep(10 * time.Millisecond)
	}
	deltas, err := a.CollectTraffic(t.Context())
	if err != nil || len(deltas) != 2 || a.TrafficMeterStatus().Mode != "kernel_counter" {
		t.Fatal("closed user traffic not counted", deltas, err)
	}
	for _, delta := range deltas {
		if delta.Up == 0 || delta.Down < 5*int64(len("closed-short-connection")) {
			t.Fatal("short payload missing", delta)
		}
	}
	if again, err := a.CollectTraffic(t.Context()); err != nil || len(again) != 0 {
		t.Fatal("duplicate billing", again, err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 128)
		n, from, err := pc.ReadFrom(buf)
		if err == nil {
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
	udpPort := pc.LocalAddr().(*net.UDPAddr).Port
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(clientPort)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	uuid, _ := hex.DecodeString(strings.ReplaceAll(in.Users[0].UUID, "-", ""))
	wire := append([]byte{0}, uuid...)
	wire = append(wire, 0, 2, byte(udpPort>>8), byte(udpPort), 1, 127, 0, 0, 1, 0, 4, 'p', 'i', 'n', 'g')
	if _, err = c.Write(wire); err != nil {
		t.Fatal(err)
	}
	var header [2]byte
	if _, err = io.ReadFull(c, header[:]); err != nil {
		t.Fatal(err)
	}
	io.CopyN(io.Discard, c, int64(header[1]))
	if _, err = io.ReadFull(c, header[:]); err != nil || header != [2]byte{0, 4} {
		t.Fatal(header, err)
	}
	var payload [4]byte
	if _, err = io.ReadFull(c, payload[:]); err != nil || string(payload[:]) != "ping" {
		t.Fatal(payload, err)
	}
	c.Close()
	deltas, err = a.CollectTraffic(t.Context())
	if err != nil || len(deltas) != 1 || deltas[0].UserID != in.Users[0].ID || deltas[0].Up != 4 || deltas[0].Down != 4 {
		t.Fatal("UDP count was lost, double billed or attributed to another user", deltas, err)
	}
}

func TestNativeCounterEpochAndFailures(t *testing.T) {
	var mu sync.Mutex
	state := map[string]any{"version": 1, "epoch": strings.Repeat("a", 32), "users": map[string]userTotals{"alice": {Up: 10, Down: 20}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"polarisUserTraffic": state})
	}))
	defer server.Close()
	a := New(core.Options{APIListen: strings.TrimPrefix(server.URL, "http://")})
	if out, err := a.CollectTraffic(t.Context()); err != nil || len(out) != 1 || out[0].Up != 10 {
		t.Fatal(out, err)
	}
	mu.Lock()
	state["users"] = map[string]userTotals{"alice": {Up: 5, Down: 20}}
	mu.Unlock()
	if out, err := a.CollectTraffic(t.Context()); err == nil || len(out) != 0 {
		t.Fatal("counter rollback accepted", out, err)
	}
	mu.Lock()
	state["epoch"] = strings.Repeat("b", 32)
	mu.Unlock()
	if out, err := a.CollectTraffic(t.Context()); err != nil || len(out) != 1 || out[0].Up != 5 {
		t.Fatal("new process not counted", out, err)
	}
	mu.Lock()
	state = nil
	mu.Unlock()
	if out, err := a.CollectTraffic(t.Context()); err == nil || len(out) != 0 {
		t.Fatal("native API loss silently fell back to sampling", out, err)
	}
}

func TestTUICCountersAndOnlineIPsUsePanelIdentity(t *testing.T) {
	in := coretest.Inbound(model.CoreMihomo, "tuic", 12345, model.ProtocolSpec{Type: model.ProtoTUIC}, model.TransportSpec{}, coretest.TLS(t, "test.example"))
	user := in.Users[0]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := connection{ID: "tuic-connection"}
		c.Metadata.InboundUser, c.Metadata.SourceIP = user.UUID, "192.0.2.10"
		json.NewEncoder(w).Encode(map[string]any{"connections": []connection{c}, "polarisUserTraffic": map[string]any{"version": 1, "epoch": strings.Repeat("c", 32), "users": map[string]userTotals{user.UUID: {Up: 7, Down: 11}, user.ID: {Up: 3, Down: 5}}}})
	}))
	defer server.Close()
	a := New(core.Options{APIListen: strings.TrimPrefix(server.URL, "http://")})
	if _, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}}); err != nil {
		t.Fatal(err)
	}
	// Final closed-connection readings can arrive after the user is removed.
	if _, err := a.Render(&model.NodeSpec{}); err != nil {
		t.Fatal(err)
	}
	if out, err := a.CollectTraffic(t.Context()); err != nil || len(out) != 1 || out[0].UserID != user.ID || out[0].Up != 10 || out[0].Down != 16 {
		t.Fatal("TUIC credentials were billed as a UUID", out, err)
	}
	if ips, err := a.OnlineIPs(t.Context()); err != nil || len(ips[user.ID]) != 1 || ips[user.ID][0] != "192.0.2.10" {
		t.Fatal("TUIC online IP belonged to wrong user", ips, err)
	}
}
