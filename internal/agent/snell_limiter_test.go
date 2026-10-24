//go:build pro

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/core/singbox"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"golang.org/x/net/proxy"
)

func TestSnellLimiterNativeCounting(t *testing.T) {
	for _, mode := range []string{"userkey", "psk", "psk-ipv4"} {
		t.Run(mode, func(t *testing.T) { snellLimiterNativeCounting(t, mode) })
	}
}
func snellLimiterNativeCounting(t *testing.T, mode string) {
	bin := os.Getenv("POLARIS_SNELL_BIN")
	if bin == "" {
		t.Skip("set POLARIS_SNELL_BIN")
	}
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lim := newLimiter(dir, log)
	defer lim.close()
	wire := lim.wire(fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t)), fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t)))
	if wire == nil {
		t.Fatal("limiter unavailable")
	}
	users := coretest.Users()
	port := coretest.FreePort(t)
	in := coretest.Inbound(model.CoreSingbox, "snell", port, model.ProtocolSpec{Type: model.ProtoSnell, SnellVersion: 5, ServerKey: "snell-server-encryption-key"}, model.TransportSpec{}, model.SecuritySpec{})
	in.Listen = "127.0.0.1"
	in.Sniffing.Enabled = false
	spec := &model.NodeSpec{Inbounds: []model.Inbound{in}, UserLimits: []model.UserLimit{{User: users[0].ID, UpKbps: 8000, DownKbps: 8000}}}
	if mode != "userkey" {
		spec.Inbounds = append(spec.Inbounds, spec.Inbounds[0])
		for i := range spec.Inbounds {
			spec.Inbounds[i].Users = []model.User{users[i]}
			spec.Inbounds[i].Protocol.SnellAuth = "psk"
			spec.Inbounds[i].Protocol.SnellUserID = users[i].ID
		}
		spec.Inbounds[1].Tag = "snell-second"
		spec.Inbounds[1].Port = coretest.FreePort(t)
		if mode == "psk-ipv4" {
			for i := range spec.Inbounds {
				spec.Inbounds[i].OutboundIPStrategy = model.OutboundIPv4Only
			}
		}
	}
	lim.setLimits(spec.UserLimits)
	a := singbox.New(core.Options{BinPath: bin, Version: "1.14.4", WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t)), Limit: wire, Logger: log})
	b, e := a.Render(spec)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Validate(b); e != nil {
		t.Fatal(e)
	}
	if e = core.AtomicWrite(filepath.Join(dir, "singbox.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	if e = a.Start(t.Context()); e != nil {
		t.Fatal(e)
	}
	defer a.Stop(context.Background())
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	payload := bytes.Repeat([]byte("data"), 1<<16)
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				b := make([]byte, len(payload))
				if _, e := io.ReadFull(c, b); e == nil {
					c.Write(b)
				}
			}()
		}
	}()
	udp, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer udp.Close()
	go func() {
		b := make([]byte, 4096)
		for {
			n, peer, e := udp.ReadFrom(b)
			if e != nil {
				return
			}
			udp.WriteTo(b[:n], peer)
		}
	}()
	udpPayload := []byte("limited-snell-udp")
	expected := int64(len(payload) + len(udpPayload))
	for _, u := range users {
		socks := coretest.FreePort(t)
		out := sub.SingboxOutbound(sub.Node{Name: "snell", Protocol: model.ProtoSnell, Server: "127.0.0.1", Port: port, SnellVersion: 4, SnellPSK: in.Protocol.ServerKey, SnellUserKey: u.Password})
		if mode != "userkey" {
			index := 0
			if u.ID == users[1].ID {
				index = 1
			}
			inbound := spec.Inbounds[index]
			out = sub.SingboxOutbound(sub.Node{Name: "snell", Protocol: model.ProtoSnell, Server: "127.0.0.1", Port: inbound.Port, SnellVersion: 4, SnellSinglePSK: true, SnellPSK: inbound.SnellClientPSK(u)})
		}
		cfg, _ := json.Marshal(map[string]any{"log": map[string]any{"level": "error"}, "inbounds": []any{map[string]any{"type": "socks", "listen": "127.0.0.1", "listen_port": socks}}, "outbounds": []any{out}})
		p := filepath.Join(dir, u.ID+".json")
		os.WriteFile(p, cfg, 0600)
		cmd := exec.Command(bin, "run", "-c", p)
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		waitTCP(t, fmt.Sprintf("127.0.0.1:%d", socks))
		d, _ := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", socks), nil, &net.Dialer{Timeout: time.Second})
		c, e := d.Dial("tcp", ln.Addr().String())
		if e != nil {
			t.Fatal(e)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, e = c.Write(payload); e != nil {
			t.Fatal(e)
		}
		b := make([]byte, len(payload))
		_, e = io.ReadFull(c, b)
		c.Close()
		if e != nil || !bytes.Equal(b, payload) {
			t.Fatal("limited Snell payload failed", e)
		}
		if e := coretest.SOCKSUDP(socks, udp.LocalAddr().String(), udpPayload); e != nil {
			t.Fatal("limited Snell UDP failed", e)
		}
	}
	totals := map[string][2]int64{}
	limited := [2]int64{}
	for tries := 0; tries < 40; tries++ {
		ds, e := a.CollectTraffic(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		for _, d := range ds {
			if strings.HasPrefix(d.UserID, "outbound>>>"+model.LimitOutboundPrefix) {
				limited[0] += d.Up
				limited[1] += d.Down
			}
			if d.UserID == "u1" || d.UserID == "u2" {
				v := totals[d.UserID]
				v[0] += d.Up
				v[1] += d.Down
				totals[d.UserID] = v
			}
		}
		if totals["u1"][1] >= expected && totals["u2"][1] >= expected && limited[1] >= expected {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if limited != [2]int64{expected, expected} {
		t.Fatal("limited user bypassed actual limiter detour", limited)
	}
	for _, u := range users {
		if totals[u.ID] != [2]int64{expected, expected} {
			t.Fatalf("limiter double billed or lost Snell identity %s %v", u.ID, totals[u.ID])
		}
	}
}
