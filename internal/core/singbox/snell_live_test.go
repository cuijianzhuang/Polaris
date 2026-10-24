package singbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"golang.org/x/net/proxy"
)

// Run with a pinned sing-box 1.14.4 binary built with with_v2ray_api.
// Both endpoints and traffic stay on loopback; counts are real routed payloads.
func TestSnellNativeAuthTrafficAndRevocation(t *testing.T) {
	bin := os.Getenv("POLARIS_SNELL_BIN")
	if bin == "" {
		t.Skip("set POLARIS_SNELL_BIN to sing-box >=1.14 with v2ray_api")
	}
	for _, tc := range []struct {
		name       string
		version    int
		obfs, mode string
	}{
		{"v5", 5, "none", ""}, {"v5-http", 5, "http", ""}, {"v6", 6, "", "default"}, {"v6-unshaped", 6, "", "unshaped"}, {"v6-raw", 6, "", "unsafe-raw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := snellSpec(t)
			in := &s.Inbounds[0]
			in.Protocol.SnellVersion, in.Protocol.SnellObfs, in.Protocol.SnellMode = tc.version, tc.obfs, tc.mode
			dir := t.TempDir()
			a := New(core.Options{BinPath: bin, Version: "1.14.4", WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))})
			if !a.PerUserTraffic() {
				t.Fatal("Snell test requires real user statistics")
			}
			write := func() {
				t.Helper()
				cfg, err := a.Render(s)
				if err != nil {
					t.Fatal(err)
				}
				if err = a.Validate(cfg); err != nil {
					t.Fatal(err)
				}
				if err = core.AtomicWrite(a.configPath(), cfg, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write()
			if err := a.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Stop(context.Background()) })
			snellReady(t, a, ctx)
			up, down := bytes.Repeat([]byte("U"), 65536), bytes.Repeat([]byte("D"), 131072)
			target, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			var hits atomic.Int64
			go func() {
				for {
					c, e := target.Accept()
					if e != nil {
						return
					}
					hits.Add(1)
					go func() {
						defer c.Close()
						c.SetDeadline(time.Now().Add(3 * time.Second))
						b := make([]byte, len(up))
						if _, e := io.ReadFull(c, b); e == nil && bytes.Equal(b, up) {
							c.Write(down)
						}
					}()
				}
			}()
			packet, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer packet.Close()
			go func() {
				b := make([]byte, 4096)
				for {
					n, peer, e := packet.ReadFrom(b)
					if e != nil {
						return
					}
					packet.WriteTo(b[:n], peer)
				}
			}()
			client := func(key string) int {
				t.Helper()
				port := coretest.FreePort(t)
				node := sub.Node{Name: "snell", Server: "127.0.0.1", Port: in.Port, Protocol: model.ProtoSnell, SnellVersion: 4, SnellPSK: in.Protocol.ServerKey, SnellUserKey: key, SnellObfs: tc.obfs, SnellMode: tc.mode}
				if tc.version == 6 {
					node.SnellVersion = 6
				}
				// The negative no-userkey client is deliberately built manually from the
				// exact exported config: production exports refuse missing user keys.
				node.SnellUserKey = "placeholder"
				outbound := sub.SingboxOutbound(node)
				outbound["userkey"] = key
				cfg := map[string]any{"log": map[string]any{"level": "error"}, "inbounds": []any{map[string]any{"type": "socks", "tag": "local", "listen": "127.0.0.1", "listen_port": port}}, "outbounds": []any{outbound}, "route": map[string]any{"final": "snell"}}
				b, _ := json.Marshal(cfg)
				p := filepath.Join(dir, fmt.Sprintf("client-%d.json", port))
				os.WriteFile(p, b, 0600)
				cmd := exec.Command(bin, "run", "-c", p)
				var log bytes.Buffer
				cmd.Stdout, cmd.Stderr = &log, &log
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
				deadline := time.Now().Add(4 * time.Second)
				for time.Now().Before(deadline) {
					c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
					if e == nil {
						c.Close()
						return port
					}
					time.Sleep(20 * time.Millisecond)
				}
				t.Fatal("client did not start")
				return 0
			}
			p1, p2, wrong, bare := client(in.Users[0].Password), client(in.Users[1].Password), client("wrong-key"), client("")
			transfer := func(port int) error {
				d, e := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", port), nil, &net.Dialer{Timeout: time.Second})
				if e != nil {
					return e
				}
				c, e := d.Dial("tcp", target.Addr().String())
				if e != nil {
					return e
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(1500 * time.Millisecond))
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
			for _, p := range []int{p1, p2} {
				if err := transfer(p); err != nil {
					t.Fatal("authorised TCP", err)
				}
			}
			payload := bytes.Repeat([]byte("udp"), 64)
			for _, p := range []int{p1, p2} {
				if err := snellUDP(p, packet.LocalAddr().String(), payload); err != nil {
					t.Fatal("authorised UDP", err)
				}
			}
			counts := map[string][2]int64{}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				ds, e := a.CollectTraffic(ctx)
				if e != nil {
					t.Fatal(e)
				}
				for _, d := range ds {
					if d.UserID == "u1" || d.UserID == "u2" {
						v := counts[d.UserID]
						v[0] += d.Up
						v[1] += d.Down
						counts[d.UserID] = v
					}
				}
				if counts["u1"][1] >= int64(len(down)+len(payload)) && counts["u2"][1] >= int64(len(down)+len(payload)) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			for _, id := range []string{"u1", "u2"} {
				want := [2]int64{int64(len(up) + len(payload)), int64(len(down) + len(payload))}
				if counts[id] != want {
					t.Fatalf("%s actual payload count %v want %v (must not double bill)", id, counts[id], want)
				}
			}
			for _, p := range []int{wrong, bare} {
				if transfer(p) == nil {
					t.Fatal("unknown or absent user key accepted")
				}
			}
			if hits.Load() != 2 {
				t.Fatal("unauthorised request reached business target", hits.Load())
			}
			// Keep an authenticated session open across the user removal.
			hold, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer hold.Close()
			go func() {
				for {
					conn, e := hold.Accept()
					if e != nil {
						return
					}
					go func() { defer conn.Close(); io.Copy(conn, conn) }()
				}
			}()
			dialer, _ := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", p1), nil, &net.Dialer{Timeout: time.Second})
			active, e := dialer.Dial("tcp", hold.Addr().String())
			if e != nil {
				t.Fatal(e)
			}
			defer active.Close()
			active.SetDeadline(time.Now().Add(time.Second))
			active.Write([]byte("ping"))
			var pong [4]byte
			if _, e = io.ReadFull(active, pong[:]); e != nil || string(pong[:]) != "ping" {
				t.Fatal("active session setup failed", e)
			}
			// Flush counters as the Agent does before replacing the core config.
			a.CollectTraffic(ctx)
			s.Inbounds[0].Users = s.Inbounds[0].Users[1:]
			write()
			if err := a.Reload(ctx); err != nil {
				t.Fatal(err)
			}
			snellReady(t, a, ctx)
			active.SetDeadline(time.Now().Add(time.Second))
			_, writeErr := active.Write([]byte("ping"))
			_, readErr := io.ReadFull(active, pong[:])
			if writeErr == nil && readErr == nil {
				t.Fatal("revoked user's established session survived reload")
			}
			if transfer(p1) == nil {
				t.Fatal("revoked user still connects after reload")
			}
			if err := transfer(p2); err != nil {
				t.Fatal("remaining user lost access", err)
			}
			s.Inbounds[0].Users = nil
			write()
			if err := a.Reload(ctx); err != nil {
				t.Fatal(err)
			}
			snellReady(t, a, ctx)
			if transfer(p2) == nil || transfer(bare) == nil {
				t.Fatal("empty authorised set enabled shared PSK access")
			}
		})
	}
}

func snellReady(t *testing.T, a *Adapter, ctx context.Context) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := a.CollectTraffic(ctx); err == nil && a.Health(ctx).Running {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("stats/core did not become healthy", a.Health(ctx))
}

func snellUDP(port int, target string, payload []byte) error {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte{5, 1, 0})
	var hello [2]byte
	if _, err = io.ReadFull(c, hello[:]); err != nil {
		return err
	}
	c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	var header [4]byte
	if _, err = io.ReadFull(c, header[:]); err != nil {
		return err
	}
	if header[1] != 0 {
		return fmt.Errorf("SOCKS UDP associate rejected")
	}
	n := 4
	if header[3] == 4 {
		n = 16
	} else if header[3] != 1 {
		return fmt.Errorf("unexpected bind family")
	}
	bind := make([]byte, n+2)
	if _, err = io.ReadFull(c, bind); err != nil {
		return err
	}
	u, err := net.Dial("udp", net.JoinHostPort(net.IP(bind[:n]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(bind[n:])))))
	if err != nil {
		return err
	}
	defer u.Close()
	u.SetDeadline(time.Now().Add(2 * time.Second))
	host, p, _ := net.SplitHostPort(target)
	pn, _ := strconv.Atoi(p)
	packet := append([]byte{0, 0, 0, 1}, net.ParseIP(host).To4()...)
	packet = append(packet, byte(pn>>8), byte(pn))
	packet = append(packet, payload...)
	if _, err = u.Write(packet); err != nil {
		return err
	}
	b := make([]byte, 4096)
	nr, err := u.Read(b)
	if err != nil {
		return err
	}
	if nr < 10 || !bytes.Equal(b[10:nr], payload) {
		return fmt.Errorf("UDP payload mismatch")
	}
	return nil
}
