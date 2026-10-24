package singbox

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
		single     bool
	}{
		{"v5", 5, "none", "", false}, {"v5-http", 5, "http", "", false}, {"v6", 6, "", "default", false}, {"v6-unshaped", 6, "", "unshaped", false}, {"v6-raw", 6, "", "unsafe-raw", false},
		{"psk-v5", 5, "none", "", true}, {"psk-v5-http", 5, "http", "", true}, {"psk-v6", 6, "", "default", true}, {"psk-v6-unshaped", 6, "", "unshaped", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := snellSpec(t)
			in := &s.Inbounds[0]
			in.Protocol.SnellVersion, in.Protocol.SnellObfs, in.Protocol.SnellMode = tc.version, tc.obfs, tc.mode
			authorised := coretest.Users()
			if tc.single {
				s.Inbounds = append(s.Inbounds, s.Inbounds[0])
				in = &s.Inbounds[0]
				for i := range s.Inbounds {
					s.Inbounds[i].Users = []model.User{authorised[i]}
					s.Inbounds[i].Protocol.SnellAuth = "psk"
					s.Inbounds[i].Protocol.SnellUserID = authorised[i].ID
				}
				s.Inbounds[1].Tag = "snell-second"
				s.Inbounds[1].ID = "snell-second"
				s.Inbounds[1].Port = coretest.FreePort(t)
			}

			s.Routes = []model.RouteRule{{Priority: 1, Match: model.RouteMatch{Users: []string{"u1"}, IPCIDR: []string{"127.0.0.2/32"}}, Outbound: "block"}}
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
				if tc.single {
					source := in
					u := authorised[0]
					if key == authorised[1].Password {
						source = &s.Inbounds[1]
						u = authorised[1]
					}
					node.SnellSinglePSK = true
					node.SnellUserKey = ""
					node.Port = source.Port
					node.SnellPSK = source.SnellClientPSK(u)
					if key == "wrong-key" {
						node.SnellPSK = "wrong-encryption-key"
					} else if key == "" {
						node.SnellPSK = source.Protocol.ServerKey
					} else if key == "cross-user" {
						node.SnellPSK = in.SnellClientPSK(authorised[1])
					}
					outbound = sub.SingboxOutbound(node)
				}

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
			p1, p2, wrong, bare := client(authorised[0].Password), client(authorised[1].Password), client("wrong-key"), client("")
			transfer := func(port int, destinations ...string) error {
				d, e := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", port), nil, &net.Dialer{Timeout: time.Second})
				if e != nil {
					return e
				}
				destination := target.Addr().String()
				if len(destinations) > 0 {
					destination = destinations[0]
				}
				c, e := d.Dial("tcp", destination)
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
			payload := bytes.Repeat([]byte("udp"), 64)
			for _, p := range []int{p1, p2} {
				if err := coretest.SOCKSUDP(p, packet.LocalAddr().String(), payload); err != nil {
					t.Fatal("authorised UDP", err)
				}
			}
			// Log attribution is asynchronous with delivery of the UDP reply.
			var ipsBeforeTCP map[string][]string
			sourceDeadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(sourceDeadline) {
				ipsBeforeTCP, _ = a.OnlineIPs(ctx)
				if len(ipsBeforeTCP["u1"]) > 0 && len(ipsBeforeTCP["u2"]) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(ipsBeforeTCP["u1"]) == 0 || len(ipsBeforeTCP["u2"]) == 0 {
				t.Fatal("UDP-only source attribution lost", ipsBeforeTCP)
			}
			for _, p := range []int{p1, p2} {
				if err := transfer(p); err != nil {
					t.Fatal("authorised TCP", err)
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
			negatives := []int{wrong, bare}
			if tc.single {
				negatives = append(negatives, client("cross-user"))
			}
			for _, p := range negatives {
				if transfer(p) == nil {
					t.Fatal("unknown or absent user key accepted")
				}
			}
			if hits.Load() != 2 {
				t.Fatal("unauthorised request reached business target", hits.Load())
			}
			// A user's routing policy must also match anonymous-metadata PSK
			// listeners. Another user may still reach the same local target.
			otherTarget, e := net.Listen("tcp", "127.0.0.2:0")
			if e != nil {
				t.Fatal(e)
			}
			defer otherTarget.Close()
			go func() {
				for {
					conn, e := otherTarget.Accept()
					if e != nil {
						return
					}
					go func() {
						defer conn.Close()
						conn.SetDeadline(time.Now().Add(2 * time.Second))
						b := make([]byte, len(up))
						if _, e := io.ReadFull(conn, b); e == nil {
							conn.Write(down)
						}
					}()
				}
			}()
			if transfer(p1, otherTarget.Addr().String()) == nil {
				t.Fatal("owner-based routing policy was bypassed")
			}
			if e := transfer(p2, otherTarget.Addr().String()); e != nil {
				t.Fatal("another user's route was incorrectly blocked", e)
			}
			ips, e := a.OnlineIPs(ctx)
			if e != nil || len(ips["u1"]) == 0 || len(ips["u2"]) == 0 {
				t.Fatal("authenticated source attribution lost", ips, e)
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
			// Render the replacement first, then drain the still-running core,
			// exactly as the Agent does. Pending ownership must not steal bytes.
			if tc.single {
				s.Inbounds[0].Users = nil
			} else {
				s.Inbounds[0].Users = s.Inbounds[0].Users[1:]
			}
			write()
			ds, err := a.CollectTraffic(ctx)
			if err != nil {
				t.Fatal(err)
			}
			oldOwnerFound := false
			for _, d := range ds {
				if d.UserID == "u1" && d.Up >= 4 && d.Down >= 4 {
					oldOwnerFound = true
				}
			}
			if !oldOwnerFound {
				t.Fatal("rendering replacement lost the previous owner's last traffic")
			}
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
			for i := range s.Inbounds {
				s.Inbounds[i].Users = nil
			}
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
