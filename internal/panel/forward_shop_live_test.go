package panel

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/core/gost"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A user's forward runs on real GOST: traffic passes, GOST's per-rule
// counters come back under the rule's tag, and settling charges the
// owner's forwarding allowance.
func TestUserForwardLiveGost(t *testing.T) {
	gbin := os.Getenv("POLARIS_GOST_BIN")
	if gbin == "" {
		t.Skip("POLARIS_GOST_BIN not set")
	}
	s, _ := fwdFixture(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	// Loopback targets are refused to users; the test plants the rule.
	port := coretest.FreePort(t)
	r := store.ForwardRule{ID: store.NewID(), NodeID: "n1", UserID: "u1", Engine: model.EngineGost, Name: "live", ListenIP: "127.0.0.1", ListenPort: port, Network: "tcp",
		Remotes: store.NewJSON([]store.ForwardRemote{{Addr: echo.Addr().String(), Enabled: true, Weight: 1}}), Enabled: true, Health: store.NewJSON(map[string]bool{})}
	s.st.DB.Create(&r)
	spec, err := s.BuildSpec("n1")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a := gost.New(core.Options{Version: "3.2.6", BinPath: gbin, WorkDir: dir, APIListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(coretest.FreePort(t)))})
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.AtomicWrite(filepath.Join(dir, "gost.yaml"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())

	payload := make([]byte, 64<<10)
	deadline := time.Now().Add(10 * time.Second)
	var c net.Conn
	for c == nil {
		if c, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err != nil {
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			c = nil
			time.Sleep(200 * time.Millisecond)
		}
	}
	c.Write(payload)
	got := make([]byte, len(payload))
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("echo through the user's forward: %v", err)
	}
	c.Close()

	var deltas []model.TrafficDelta
	for time.Now().Before(deadline.Add(10 * time.Second)) {
		ds, err := a.CollectTraffic(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		deltas = append(deltas, ds...)
		var sum int64
		for _, d := range deltas {
			if d.UserID == "inbound>>>fwd-"+r.ID {
				sum += d.Up + d.Down
			}
		}
		if sum >= int64(2*len(payload)) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := s.traffic.Add("n1", deltas); err != nil {
		t.Fatal(err)
	}
	if err := s.settleTraffic(); err != nil {
		t.Fatal(err)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "u1")
	want := int64(float64(2*len(payload)) * 1.5 * 2) // node rate x plan rate
	if u.FwdUsed < want || u.FwdUsed > want*11/10 || u.UsedUp+u.UsedDown != 0 {
		t.Fatalf("charged %d forward bytes (want ~%d), proxy %d; deltas %+v", u.FwdUsed, want, u.UsedUp+u.UsedDown, deltas)
	}
}
