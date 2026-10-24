package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/core/singbox"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"golang.org/x/net/proxy"
)

// This is the actual panel -> core -> collector -> inbox -> rate -> quota
// path, not fabricated traffic numbers. The native core test also covers UDP.
func TestSnellNativePanelBillingQuotaAndSubscription(t *testing.T) {
	bin := os.Getenv("POLARIS_SNELL_BIN")
	if bin == "" {
		t.Skip("set POLARIS_SNELL_BIN")
	}
	s, _, _ := snellPanelFixture(t)
	dir := t.TempDir()
	api := fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))
	adapter := singbox.New(core.Options{BinPath: bin, Version: "1.14.4", WorkDir: dir, APIListen: api})
	write := func() {
		t.Helper()
		if _, _, err := s.Publish("snell-node", "Snell native test", false); err != nil {
			t.Fatal(err)
		}
		spec, e := s.BuildSpec("snell-node")
		if e != nil {
			t.Fatal(e)
		}
		b, e := adapter.Render(spec)
		if e != nil {
			t.Fatal(e)
		}
		if e = adapter.Validate(b); e != nil {
			t.Fatal(e)
		}
		if e = core.AtomicWrite(filepath.Join(dir, "singbox.json"), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write()
	if e := adapter.Start(t.Context()); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { adapter.Stop(context.Background()) })
	time.Sleep(150 * time.Millisecond)
	clients := map[string]int{}
	for _, u := range coretest.Users() {
		var row store.User
		s.st.DB.First(&row, "id = ?", u.ID)
		nodes, e := s.subNodes(&row)
		if e != nil || len(nodes) != 1 {
			t.Fatal(e, nodes)
		}
		if nodes[0].SnellUserKey != u.Password {
			t.Fatal("subscription did not use independent credential")
		}
		out := sub.SingboxOutbound(nodes[0])
		port := coretest.FreePort(t)
		b, _ := json.Marshal(map[string]any{"log": map[string]any{"level": "error"}, "inbounds": []any{map[string]any{"type": "socks", "listen": "127.0.0.1", "listen_port": port}}, "outbounds": []any{out}})
		p := filepath.Join(dir, u.ID+".json")
		os.WriteFile(p, b, 0600)
		cmd := exec.Command(bin, "run", "-c", p)
		if e := cmd.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		deadline := time.Now().Add(4 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
			if err == nil {
				conn.Close()
				ready = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !ready {
			t.Fatal("Snell subscription client did not start")
		}
		clients[u.ID] = port
	}
	time.Sleep(150 * time.Millisecond)
	up, down := bytes.Repeat([]byte("U"), 16384), bytes.Repeat([]byte("D"), 32768)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(2 * time.Second))
				b := make([]byte, len(up))
				if _, e := io.ReadFull(c, b); e == nil && bytes.Equal(b, up) {
					c.Write(down)
				}
			}()
		}
	}()
	transfer := func(port int) error {
		d, e := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", port), nil, &net.Dialer{Timeout: time.Second})
		if e != nil {
			return e
		}
		c, e := d.Dial("tcp", ln.Addr().String())
		if e != nil {
			return e
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(time.Second))
		if _, e = c.Write(up); e != nil {
			return e
		}
		b := make([]byte, len(down))
		_, e = io.ReadFull(c, b)
		if e == nil && !bytes.Equal(b, down) {
			return fmt.Errorf("payload mismatch")
		}
		return e
	}
	for _, port := range clients {
		if e := transfer(port); e != nil {
			t.Fatal(e)
		}
	}
	totals := map[string][2]int64{}
	for attempt := 0; attempt < 40; attempt++ {
		ds, e := adapter.CollectTraffic(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		for _, d := range ds {
			if _, ok := clients[d.UserID]; ok {
				v := totals[d.UserID]
				v[0] += d.Up
				v[1] += d.Down
				totals[d.UserID] = v
			}
		}
		if totals["u1"][1] >= int64(len(down)) && totals["u2"][1] >= int64(len(down)) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	b := &polarisv1.TrafficBatch{BatchId: "snell-native-1", Epoch: "native", Seq: 1}
	for _, id := range []string{"u1", "u2"} {
		v := totals[id]
		if v != [2]int64{int64(len(up)), int64(len(down))} {
			t.Fatal("native counts wrong", id, v)
		}
		b.Users = append(b.Users, &polarisv1.UserTraffic{UserId: id, Up: v[0], Down: v[1]})
	}
	allowed := s.nodeUsers("snell-node")
	for i := 0; i < 2; i++ {
		if _, _, e := s.ingestTraffic("snell-node", []*polarisv1.TrafficBatch{b}, allowed); e != nil {
			t.Fatal(e)
		}
		if e := s.settleTraffic(); e != nil {
			t.Fatal(e)
		}
	}
	for _, id := range []string{"u1", "u2"} {
		var u store.User
		s.st.DB.First(&u, "id = ?", id)
		if u.UsedUp != int64(2*len(up)) || u.UsedDown != int64(2*len(down)) {
			t.Fatalf("rate/dedup billing wrong %s: %d/%d", id, u.UsedUp, u.UsedDown)
		}
	}
	s.enforceLimits()
	spec, e := s.BuildSpec("snell-node")
	if e != nil || len(spec.Inbounds[0].Users) != 1 || spec.Inbounds[0].Users[0].ID != "u2" {
		t.Fatal("exhausted user retained in core spec", e)
	}
	var exhausted store.User
	s.st.DB.First(&exhausted, "id = ?", "u1")
	nodes, e := s.subNodes(&exhausted)
	if e != nil || len(nodes) != 0 {
		t.Fatal("exhausted user still receives Snell subscription", e)
	}
	write()
	if e := adapter.Reload(t.Context()); e != nil {
		t.Fatal(e)
	}
	time.Sleep(200 * time.Millisecond)
	if transfer(clients["u1"]) == nil {
		t.Fatal("exhausted user stale credential accepted")
	}
	if e := transfer(clients["u2"]); e != nil {
		t.Fatal("remaining user blocked", e)
	}
}
