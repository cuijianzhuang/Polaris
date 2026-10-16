package xray

import (
	"context"
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
	"github.com/cuijianzhuang/polaris/internal/model"
)

// TestLiveTrafficHotUser counts the traffic of a user added through the gRPC
// API (the path every new panel user takes) on a real Xray.
func TestLiveTrafficHotUser(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_BIN")
	if bin == "" {
		t.Skip("POLARIS_XRAY_BIN not set")
	}
	ver := os.Getenv("POLARIS_XRAY_VERSION")
	if ver == "" {
		ver = "26.3.27"
	}
	api := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	dir := t.TempDir()
	a := New(core.Options{Version: ver, BinPath: bin, WorkDir: dir, APIListen: api})
	port := freePort(t)
	in := coretest.Inbound(model.CoreXray, "vless", port, model.ProtocolSpec{Type: model.ProtoVLESS},
		model.TransportSpec{}, model.SecuritySpec{})
	in.Listen = "127.0.0.1"
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}})
	if err != nil {
		t.Fatal(err)
	}
	// Recent Xray blocks private targets from freedom; the test target is local.
	var m map[string]any
	json.Unmarshal(cfg, &m)
	for _, o := range m["outbounds"].([]any) {
		if ob := o.(map[string]any); ob["protocol"] == "freedom" {
			st, _ := ob["settings"].(map[string]any)
			if st == nil {
				st = map[string]any{}
			}
			st["finalRules"] = []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.0/8"}}}
			ob["settings"] = st
		}
	}
	cfg, _ = json.Marshal(m)
	core.AtomicWrite(a.configPath(), cfg, 0o600)
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(ctx)
	for i := 0; ; i++ {
		if c, err := net.DialTimeout("tcp", api, 100*time.Millisecond); err == nil {
			c.Close()
			break
		} else if i == 50 {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	hot := model.User{ID: "hotuser", UUID: "11111111-2222-4333-8444-555555555555"}
	if err := a.AddUsers(ctx, "vless", []model.User{hot}); err != nil {
		t.Fatal(err)
	}

	payload := strings.Repeat("x", 1<<20)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, payload) }))
	defer target.Close()
	socks := freePort(t)
	client := fmt.Sprintf(`{"inbounds":[{"listen":"127.0.0.1","port":%d,"protocol":"socks"}],"outbounds":[{"protocol":"vless",
		"settings":{"vnext":[{"address":"127.0.0.1","port":%d,"users":[{"id":%q,"encryption":"none"}]}]}}]}`, socks, port, hot.UUID)
	cpath := filepath.Join(dir, "client.json")
	os.WriteFile(cpath, []byte(client), 0o600)
	cl := exec.Command(bin, "run", "-c", cpath)
	if err := cl.Start(); err != nil {
		t.Fatal(err)
	}
	defer cl.Process.Kill()
	var n int
	for i := 0; i < 20 && n != len(payload); i++ {
		time.Sleep(300 * time.Millisecond)
		cmd := exec.Command("curl", "-sS", "-m", "5", "--noproxy", "", "--socks5-hostname", fmt.Sprintf("127.0.0.1:%d", socks), target.URL)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		out, _ := cmd.Output()
		n = len(out)
	}
	if n != len(payload) {
		t.Fatalf("download got %d bytes", n)
	}
	ds, err := a.CollectTraffic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var down int64
	for _, d := range ds {
		if d.UserID == hot.ID {
			down = d.Down
		}
	}
	if down < int64(len(payload)) {
		t.Fatalf("hot user traffic not counted: %+v", ds)
	}
}
