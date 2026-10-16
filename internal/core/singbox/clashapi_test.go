package singbox

import (
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
	"github.com/cuijianzhuang/polaris/internal/model"
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestLiveClashTraffic: an official sing-box (no v2ray_api) still reports
// inbound and outbound totals through its Clash API.
func TestLiveClashTraffic(t *testing.T) {
	bin, xray := os.Getenv("POLARIS_SINGBOX_BIN"), os.Getenv("POLARIS_XRAY_BIN")
	if bin == "" || xray == "" {
		t.Skip("POLARIS_SINGBOX_BIN / POLARIS_XRAY_BIN not set")
	}
	dir := t.TempDir()
	a := New(core.Options{Version: "1.13.5", BinPath: bin, WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", freePort(t))})
	if a.stats {
		t.Skip("binary has v2ray_api; the Clash API path is not used")
	}
	port := freePort(t)
	in := coretest.Inbound(model.CoreSingbox, "vin", port, model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{}, model.SecuritySpec{})
	in.Listen = "127.0.0.1"
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), `"clash_api"`) {
		t.Fatalf("no clash_api in config:\n%s", cfg)
	}
	core.AtomicWrite(a.configPath(), cfg, 0o600)
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(t.Context())

	// A slow download keeps the connection open across two polls.
	payload := strings.Repeat("x", 2<<20)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < len(payload); i += 256 << 10 {
			io.WriteString(w, payload[i:i+256<<10])
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
		}
	}))
	defer target.Close()
	socks := freePort(t)
	client := fmt.Sprintf(`{"inbounds":[{"listen":"127.0.0.1","port":%d,"protocol":"socks"}],"outbounds":[{"protocol":"vless",
		"settings":{"vnext":[{"address":"127.0.0.1","port":%d,"users":[{"id":%q,"encryption":"none"}]}]}}]}`, socks, port, in.Users[0].UUID)
	cpath := filepath.Join(dir, "client.json")
	os.WriteFile(cpath, []byte(client), 0o600)
	cl := exec.Command(xray, "run", "-c", cpath)
	if err := cl.Start(); err != nil {
		t.Fatal(err)
	}
	defer cl.Process.Kill()
	time.Sleep(time.Second)

	done := make(chan int)
	go func() {
		cmd := exec.Command("curl", "-sS", "-m", "20", "--noproxy", "", "--socks5-hostname", fmt.Sprintf("127.0.0.1:%d", socks), target.URL)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		out, _ := cmd.Output()
		done <- len(out)
	}()
	var down int64
	key := core.TagPrefixInbound + "vin"
	for polling := true; polling; {
		select {
		case n := <-done:
			if n != len(payload) {
				t.Fatalf("download got %d bytes", n)
			}
			polling = false
		case <-time.After(300 * time.Millisecond):
		}
		ds, err := a.CollectTraffic(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			if d.UserID == key {
				down += d.Down
			}
		}
	}
	// Bytes after the last poll of a connection are lost; most must be counted.
	if down < int64(len(payload))/2 {
		t.Fatalf("inbound traffic %d, want most of %d", down, len(payload))
	}
	t.Logf("counted %d of %d bytes", down, len(payload))
}
