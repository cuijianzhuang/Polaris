package coretest

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// LiveBalancerFallback exercises real forwarding and recovery without relying
// on public websites. Each local HTTP egress identifies itself in its response.
func LiveBalancerFallback(t *testing.T, a interface {
	Render(*model.NodeSpec) ([]byte, error)
	Validate([]byte) error
	Start(context.Context) error
	Stop(context.Context) error
	CollectTraffic(context.Context) ([]model.TrafficDelta, error)
}, typ model.CoreType, configPath string, primaryStrategy string, policies ...model.OutboundIPStrategy) {
	t.Helper()
	obs := []model.Outbound{}
	var alive []*atomic.Bool
	for i := 1; i <= 4; i++ {
		label := fmt.Sprintf("p%d", i)
		up := &atomic.Bool{}
		up.Store(true)
		alive = append(alive, up)
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !up.Load() {
				http.Error(w, "unavailable", http.StatusBadGateway)
				return
			}
			if r.Method != "CONNECT" {
				io.WriteString(w, label)
				return
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			rw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
			rw.Flush()
			request, err := http.ReadRequest(bufio.NewReader(rw))
			if err != nil {
				return
			}
			if request.URL.Path == "/probe" {
				rw.WriteString("HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
			} else {
				fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(label), label)
			}
			rw.Flush()
		}))
		t.Cleanup(proxy.Close)
		u, _ := url.Parse(proxy.URL)
		port, _ := strconv.Atoi(u.Port())
		obs = append(obs, model.Outbound{Tag: label, Type: model.OutHTTP, Settings: Settings(model.OutboundSettings{Server: u.Hostname(), Port: port})})
	}
	group := func(tag, fallback string, members ...string) model.Outbound {
		return model.Outbound{Tag: tag, Type: model.OutBalancer, Settings: Settings(model.OutboundSettings{Members: members, Strategy: model.LBRoundRobin, Fallback: fallback, TestURL: "http://fallback.test/probe", Interval: "1s"})}
	}
	primary := group("primary", "backup", "p1", "p2")
	ps, _ := primary.ParseSettings()
	ps.Strategy = primaryStrategy
	primary.Settings = Settings(ps)
	obs = append(obs, primary, group("backup", "last", "p3"), group("last", "", "p4"))
	port := FreePort(t)
	in := Inbound(typ, "entry", port, model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{}, model.SecuritySpec{})
	if len(policies) > 0 {
		in.OutboundIPStrategy = policies[0]
	}
	in.Listen = "127.0.0.1"
	in.Sniffing.Enabled = false
	if typ == model.CoreMihomo {
		in.Protocol.Type = model.ProtoHTTP
		in.Users = nil
	}
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}, Outbounds: obs, Routes: []model.RouteRule{
		{Match: model.RouteMatch{Inbound: []string{"entry"}}, Outbound: "primary"},
		{Priority: 100, Outbound: "block"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(cfg); err != nil {
		t.Fatalf("validate: %v\n%s", err, cfg)
	}
	if err := os.WriteFile(configPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })
	// A real authenticated VLESS user is needed to catch duplicate billing.
	uuid, _ := hex.DecodeString(strings.ReplaceAll(Users()[0].UUID, "-", ""))
	fetch := func() (string, error) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err != nil {
			return "", err
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		host := "fallback.test"
		header := append([]byte{0}, uuid...)
		header = append(header, 0, 1, 0, 80, 2, byte(len(host)))
		header = append(header, host...)
		header = append(header, []byte("GET /payload HTTP/1.1\r\nHost: fallback.test\r\nConnection: close\r\n\r\n")...)
		if _, err := conn.Write(header); err != nil {
			return "", err
		}
		var response [2]byte
		if _, err := io.ReadFull(conn, response[:]); err != nil {
			return "", err
		}
		if _, err := io.CopyN(io.Discard, conn, int64(response[1])); err != nil {
			return "", err
		}
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		b, err := io.ReadAll(io.LimitReader(res.Body, 256))
		return string(b), err
	}
	if typ == model.CoreMihomo {
		entry, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
		tr := &http.Transport{Proxy: http.ProxyURL(entry), DisableKeepAlives: true}
		t.Cleanup(tr.CloseIdleConnections)
		client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
		fetch = func() (string, error) {
			res, err := client.Get("http://fallback.test/payload")
			if err != nil {
				return "", err
			}
			defer res.Body.Close()
			b, err := io.ReadAll(io.LimitReader(res.Body, 256))
			return string(b), err
		}
	}
	measure := func() [2]int64 {
		t.Helper()
		if typ != model.CoreXray {
			return [2]int64{}
		}
		if _, err := a.CollectTraffic(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := fetch(); err != nil {
			t.Fatal(err)
		}
		stats, err := a.CollectTraffic(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var result [2]int64
		for _, d := range stats {
			if strings.Contains(d.UserID, "polaris-fallback-") {
				t.Fatalf("internal bridge traffic leaked into accounting: %+v", d)
			}
			if d.UserID == in.Users[0].ID {
				result[0] += d.Up
				result[1] += d.Down
			}
		}
		if result[0] == 0 || result[1] == 0 {
			t.Fatalf("missing real user counters: %+v", stats)
		}
		return result
	}

	waitFor := func(want ...string) {
		t.Helper()
		var got string
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			body, err := fetch()
			if err == nil {
				got = body
				for _, w := range want {
					if got == w {
						return
					}
				}
			} else {
				got = err.Error()
			}
			time.Sleep(150 * time.Millisecond)
		}
		if l, ok := a.(interface{ Logs(int) []string }); ok {
			t.Log(strings.Join(l.Logs(30), "\n"))
		}
		t.Fatalf("wanted %s, got %q", strings.Join(want, "/"), got)
	}
	waitFor("p1", "p2")
	baseline := measure()
	alive[0].Store(false)
	waitFor("p2") // A single failed member must not bypass the surviving primary.
	alive[1].Store(false)
	waitFor("p3")
	if v := measure(); v != baseline {
		t.Fatalf("one-hop fallback changed user billing: baseline=%v fallback=%v", baseline, v)
	}
	alive[2].Store(false)
	waitFor("p4") // Multi-level fallback re-enters the next balancer's strategy.
	if v := measure(); v != baseline {
		t.Fatalf("two-hop fallback changed user billing: baseline=%v fallback=%v", baseline, v)
	}
	alive[0].Store(true)
	waitFor("p1") // Recovery affects new connections without publishing a spec.
}
