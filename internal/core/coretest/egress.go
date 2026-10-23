package coretest

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/miekg/dns"
)

// FetchVLESS uses a real authenticated user, including for the limiter's
// internal re-entry tests. Targets stay local; no public probe is required.
func FetchVLESS(port int, target string) (string, error) {
	host, p, err := net.SplitHostPort(target)
	if err != nil {
		return "", err
	}
	destPort, _ := strconv.Atoi(p)
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	uuid, _ := hex.DecodeString(strings.ReplaceAll(Users()[0].UUID, "-", ""))
	header := append([]byte{0}, uuid...)
	header = append(header, 0, 1, byte(destPort>>8), byte(destPort))
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			header = append(header, 1)
			header = append(header, v4...)
		} else {
			header = append(header, 3)
			header = append(header, ip.To16()...)
		}
	} else {
		header = append(header, 2, byte(len(host)))
		header = append(header, host...)
	}
	header = append(header, []byte("GET /payload HTTP/1.1\r\nHost: egress.test\r\nConnection: close\r\n\r\n")...)
	if _, err = conn.Write(header); err != nil {
		return "", err
	}
	var response [2]byte
	if _, err = io.ReadFull(conn, response[:]); err != nil {
		return "", err
	}
	if _, err = io.CopyN(io.Discard, conn, int64(response[1])); err != nil {
		return "", err
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1024))
	return string(b), err
}

func LiveOutboundIP(t *testing.T, a interface {
	Render(*model.NodeSpec) ([]byte, error)
	Validate([]byte) error
	Start(context.Context) error
	Stop(context.Context) error
	CollectTraffic(context.Context) ([]model.TrafficDelta, error)
}, typ model.CoreType, path string, wire *model.LimitWire) {
	t.Helper()
	dnsConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsServer := &dns.Server{PacketConn: dnsConn, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		for _, q := range r.Question {
			switch q.Qtype {
			case dns.TypeA:
				m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5}, A: net.ParseIP("127.0.0.1")})
			case dns.TypeAAAA:
				m.Answer = append(m.Answer, &dns.AAAA{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 5}, AAAA: net.ParseIP("::1")})
			}
		}
		w.WriteMsg(m)
	})}
	go dnsServer.ActivateAndServe()
	t.Cleanup(func() { dnsServer.Shutdown() })
	serve := func(network, addr, label string) net.Listener {
		l, e := net.Listen(network, addr)
		if e != nil {
			t.Fatal("local dual-stack test requires IPv6:", e)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Date", "Thu, 01 Jan 2026 00:00:00 GMT")
			io.WriteString(w, label)
		})}
		go srv.Serve(l)
		t.Cleanup(func() { srv.Close() })
		return l
	}
	v4 := serve("tcp4", "127.0.0.1:0", "v4")
	port := v4.Addr().(*net.TCPAddr).Port
	serve("tcp6", fmt.Sprintf("[::1]:%d", port), "v6")
	only4 := serve("tcp4", "127.0.0.1:0", "v4").Addr().(*net.TCPAddr).Port
	only6 := serve("tcp6", "[::1]:0", "v6").Addr().(*net.TCPAddr).Port
	strategies := []model.OutboundIPStrategy{model.OutboundIPv4Only, model.OutboundIPv6Only, model.OutboundPreferIPv4, model.OutboundPreferIPv6, model.OutboundIPAuto}
	spec := &model.NodeSpec{DNS: &model.DNSSpec{Servers: []model.DNSServer{{Tag: "test", Address: "udp://" + dnsConn.LocalAddr().String()}}}}
	var ports []int
	for i, p := range strategies {
		in := Inbound(typ, fmt.Sprintf("entry-%d", i), FreePort(t), model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{}, model.SecuritySpec{})
		in.OutboundIPStrategy = p
		in.Listen = "127.0.0.1"
		in.Sniffing.Enabled = false
		spec.Inbounds = append(spec.Inbounds, in)
		ports = append(ports, in.Port)
	}
	if wire != nil {
		spec.UserLimits = []model.UserLimit{{User: Users()[0].ID, UpKbps: 8000, DownKbps: 8000}}
	}
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	// New Xray blocks private targets by default for VLESS. Allow local
	// fixtures only where a strict opposite-family block was not installed.
	if typ == model.CoreXray {
		var v map[string]any
		if err := json.Unmarshal(cfg, &v); err != nil {
			t.Fatal(err)
		}
		for _, raw := range v["outbounds"].([]any) {
			o := raw.(map[string]any)
			if o["protocol"] != "freedom" {
				continue
			}
			settings, _ := o["settings"].(map[string]any)
			if settings == nil {
				settings = map[string]any{}
				o["settings"] = settings
			}
			rules, _ := settings["finalRules"].([]any)
			settings["finalRules"] = append(rules, map[string]any{"action": "allow"})
		}
		cfg, _ = json.Marshal(v)
	}
	if err = a.Validate(cfg); err != nil {
		t.Fatalf("validate %v\n%s", err, cfg)
	}
	if dump := os.Getenv("POLARIS_EGRESS_DUMP"); dump != "" {
		os.WriteFile(dump+"-"+string(typ)+".json", cfg, 0600)
	}
	if err = os.WriteFile(path, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })
	for _, p := range ports {
		for end := time.Now().Add(5 * time.Second); ; {
			c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), time.Millisecond*100)
			if e == nil {
				c.Close()
				break
			}
			if time.Now().After(end) {
				t.Fatal("core did not start", e)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	target := func(host string, p int) string { return net.JoinHostPort(host, strconv.Itoa(p)) }
	cases := []struct {
		entry int
		host  string
		port  int
		want  string
	}{
		{0, "egress.test", port, "v4"}, {1, "egress.test", port, "v6"}, {2, "egress.test", port, "v4"}, {3, "egress.test", port, "v6"},
		{0, "::1", port, ""}, {1, "127.0.0.1", port, ""}, {0, "egress.test", only6, ""}, {1, "egress.test", only4, ""},
		{2, "egress.test", only6, "v6"}, {3, "egress.test", only4, "v4"},
		{4, "127.0.0.1", port, "v4"}, {4, "::1", port, "v6"},
	}
	var baseline [2]int64
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s-%s-%d", strategies[tc.entry], tc.host, tc.port), func(t *testing.T) {
			if typ == model.CoreXray {
				if _, err = a.CollectTraffic(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			body, e := FetchVLESS(ports[tc.entry], target(tc.host, tc.port))
			if tc.want == "" {
				if e == nil {
					t.Fatalf("strict policy escaped to %q", body)
				}
				return
			}
			if e != nil || body != tc.want {
				t.Fatalf("got %q %v, want %q", body, e, tc.want)
			}
			if typ == model.CoreXray {
				stats, e := a.CollectTraffic(t.Context())
				if e != nil {
					t.Fatal(e)
				}
				var counts [2]int64
				for _, d := range stats {
					if model.IsLimiterCounter(d.UserID) {
						continue
					}
					if strings.HasPrefix(d.UserID, "inbound>>>") || strings.HasPrefix(d.UserID, "outbound>>>") {
						continue
					}
					if d.UserID != Users()[0].ID {
						t.Fatalf("internal identity leaked to billing: %+v", d)
					}
					counts[0] += d.Up
					counts[1] += d.Down
				}
				if counts[0] == 0 || counts[1] == 0 {
					t.Fatal("missing real user counts", counts)
				}
				if baseline == [2]int64{} {
					baseline = counts
				}
				if counts != baseline {
					t.Fatalf("traffic changed across egress policies: %v vs %v", counts, baseline)
				}
			}
		})
	}
	// UDP has no connect handshake. Test family selection and strict literal
	// rejection; do not promise to replay a silently lost packet on another IP.
	udp4, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpPort := udp4.LocalAddr().(*net.UDPAddr).Port
	udp6, err := net.ListenPacket("udp6", fmt.Sprintf("[::1]:%d", udpPort))
	if err != nil {
		udp4.Close()
		t.Fatal(err)
	}
	for i, pc := range []net.PacketConn{udp4, udp6} {
		label := []string{"v4", "v6"}[i]
		t.Cleanup(func() { pc.Close() })
		go func() {
			b := make([]byte, 128)
			for {
				_, addr, e := pc.ReadFrom(b)
				if e != nil {
					return
				}
				pc.WriteTo([]byte(label), addr)
			}
		}()
	}
	for _, tc := range []struct {
		entry      int
		host, want string
	}{{0, "egress.test", "v4"}, {1, "egress.test", "v6"}, {2, "egress.test", "v4"}, {3, "egress.test", "v6"}, {0, "::1", ""}, {1, "127.0.0.1", ""}, {4, "127.0.0.1", "v4"}, {4, "::1", "v6"}} {
		t.Run(fmt.Sprintf("udp-%s-%s", strategies[tc.entry], tc.host), func(t *testing.T) {
			body, e := fetchVLESSPacket(ports[tc.entry], tc.host, udpPort)
			if tc.want == "" {
				if e == nil {
					t.Fatal("strict UDP escaped", body)
				}
				return
			}
			if e != nil || body != tc.want {
				t.Fatalf("UDP got %q %v, want %q", body, e, tc.want)
			}
		})
	}
	t.Log("authenticated traffic counters across policies:", baseline)
}

func fetchVLESSPacket(port int, host string, destPort int) (string, error) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	uuid, _ := hex.DecodeString(strings.ReplaceAll(Users()[0].UUID, "-", ""))
	header := append([]byte{0}, uuid...)
	header = append(header, 0, 2, byte(destPort>>8), byte(destPort))
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			header = append(header, 1)
			header = append(header, v4...)
		} else {
			header = append(header, 3)
			header = append(header, ip.To16()...)
		}
	} else {
		header = append(header, 2, byte(len(host)))
		header = append(header, host...)
	}
	header = append(header, 0, 4, 'p', 'i', 'n', 'g')
	if _, err = conn.Write(header); err != nil {
		return "", err
	}
	var prefix [2]byte
	if _, err = io.ReadFull(conn, prefix[:]); err != nil {
		return "", err
	}
	if _, err = io.CopyN(io.Discard, conn, int64(prefix[1])); err != nil {
		return "", err
	}
	if _, err = io.ReadFull(conn, prefix[:]); err != nil {
		return "", err
	}
	size := int(prefix[0])<<8 | int(prefix[1])
	if size > 128 {
		return "", fmt.Errorf("unexpected UDP response size %d", size)
	}
	body := make([]byte, size)
	_, err = io.ReadFull(conn, body)
	return string(body), err
}
