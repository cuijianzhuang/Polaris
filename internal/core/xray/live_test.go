package xray

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
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

// TestLiveHotUsers starts a real Xray and alters users through the gRPC API.
func TestLiveHotUsers(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_BIN")
	if bin == "" {
		t.Skip("POLARIS_XRAY_BIN not set")
	}
	api := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	dir := t.TempDir()
	a := New(core.Options{Version: "26.3.27", BinPath: bin, WorkDir: dir, APIListen: api})
	in := coretest.Inbound(model.CoreXray, "vless", freePort(t), model.ProtocolSpec{Type: model.ProtoVLESS},
		model.TransportSpec{}, model.SecuritySpec{})
	in.Listen = "127.0.0.1"
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.AtomicWrite(a.configPath(), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(ctx)
	// Wait for the gRPC API to listen instead of a fixed sleep (slow under load).
	for i := 0; ; i++ {
		c, err := net.DialTimeout("tcp", api, 100*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if i == 50 {
			t.Fatalf("xray API never came up: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	newUser := model.User{ID: "u3", UUID: "11111111-2222-4333-8444-555555555555"}
	if err := a.AddUsers(ctx, "vless", []model.User{newUser}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := a.RemoveUsers(ctx, "vless", []string{"u1"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	out, err := exec.Command(bin, "api", "inbounduser", "--server="+api, "-tag=vless").CombinedOutput()
	if err != nil {
		t.Fatalf("inbounduser: %v %s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "u3") || strings.Contains(s, `"u1"`) || !strings.Contains(s, "u2") {
		t.Fatalf("unexpected users: %s", s)
	}
	if _, err := a.CollectTraffic(ctx); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !a.Health(ctx).Running {
		t.Fatal("xray not running")
	}
}
