package sub

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Every node rendered to links, Clash or sing-box parses back to the same endpoint.
func TestRoundTrip(t *testing.T) {
	in := sample()
	for _, format := range []string{"links", "clash", "singbox"} {
		r, _ := Get(format)
		b, err := r.Render(in, Info{}, "")
		if err != nil {
			t.Fatal(err)
		}
		out, err := ParseSubscription(b)
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		got := map[string]Node{}
		for _, n := range out {
			got[n.Name] = n
		}
		for _, n := range in {
			if format == "singbox" && n.Transport.Type == model.TransXHTTP {
				continue
			}
			p, ok := got[n.Name]
			if !ok {
				t.Errorf("%s: %q missing after round trip", format, n.Name)
				continue
			}
			if p.Server != n.Server || p.Port != n.Port || p.Protocol != n.Protocol || p.UUID != n.UUID || p.Password != n.Password {
				t.Errorf("%s: %q mismatch: %+v vs %+v", format, n.Name, p, n)
			}
			if p.TLS.Type != n.TLS.Type && !(n.TLS.Type == "" && p.TLS.Type == model.SecNone) {
				t.Errorf("%s: %q tls %s vs %s", format, n.Name, p.TLS.Type, n.TLS.Type)
			}
			if n.TLS.Type == model.SecReality && (p.TLS.PublicKey != n.TLS.PublicKey || p.TLS.ShortID != n.TLS.ShortID) {
				t.Errorf("%s: reality params lost", format)
			}
		}
	}
}

func TestParseLegacyLinks(t *testing.T) {
	legacy := "ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pa:ss@1.2.3.4:8388")) + "#Legacy%20SS\n" +
		"hy2://secret@hy.example.com:443?sni=hy.example.com&obfs=salamander&obfs-password=x#🇭🇰 HK 01\n" +
		"socks://" + base64.RawURLEncoding.EncodeToString([]byte("u:p")) + "@5.6.7.8:1080#s5\n" +
		"garbage line\n"
	nodes, err := ParseSubscription([]byte(base64.StdEncoding.EncodeToString([]byte(legacy))))
	if err != nil || len(nodes) != 3 {
		t.Fatalf("%v %+v", err, nodes)
	}
	if nodes[0].Method != "aes-256-gcm" || nodes[0].Password != "pa:ss" || nodes[0].Name != "Legacy SS" {
		t.Fatalf("ss: %+v", nodes[0])
	}
	if nodes[1].Protocol != model.ProtoHysteria2 || nodes[1].ObfsPassword != "x" || NodeRegion(nodes[1]) != "HK" {
		t.Fatalf("hy2: %+v", nodes[1])
	}
	if nodes[2].Username != "u" || nodes[2].Password != "p" {
		t.Fatalf("socks: %+v", nodes[2])
	}
}

func TestRegionDetect(t *testing.T) {
	cases := map[string]string{"香港 01": "HK", "HK01": "HK", "Tokyo-2": "JP", "🇸🇬 SG": "SG", "think tank": "", "美国 洛杉矶": "US", "USA-1": "US", "UK London": "GB"}
	for name, want := range cases {
		if got := DetectRegion(name); got != want {
			t.Errorf("%q: got %q want %q", name, got, want)
		}
	}
}

func TestOperatorChain(t *testing.T) {
	nodes := []Node{
		{Name: "香港 10", Protocol: model.ProtoVLESS, Server: "a", Port: 1, UUID: "1"},
		{Name: "香港 2", Protocol: model.ProtoTrojan, Server: "b", Port: 1, Password: "x"},
		{Name: "日本 1", Protocol: model.ProtoVLESS, Server: "c", Port: 1, UUID: "1"},
		{Name: "日本 1 dup", Protocol: model.ProtoVLESS, Server: "c", Port: 1, UUID: "1"},
		{Name: "剩余流量：10GB", Protocol: model.ProtoVLESS, Server: "d", Port: 1},
		{Name: "美国 1", Protocol: model.ProtoShadowsocks, Server: "e", Port: 1, Method: "aes-128-gcm", Password: "p"},
	}
	out, err := ApplyChain(nodes, []Operator{
		{Type: "dedup"},
		{Type: "filter", DropInfo: true, Exclude: "美国"},
		{Type: "sort", SortBy: []string{"region", "name"}, RegionOrder: []string{"JP"}},
		{Type: "rename", Template: "{emoji} {regionZh} {index}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range out {
		names = append(names, n.Name)
	}
	if strings.Join(names, "|") != "🇯🇵 日本 01|🇭🇰 香港 01|🇭🇰 香港 02" {
		t.Fatalf("got %v", names)
	}
	if _, err := ApplyChain(nodes, []Operator{{Type: "filter", Include: "("}}); err == nil {
		t.Fatal("bad regex must fail")
	}
	out, _ = ApplyChain(nodes, []Operator{{Type: "filter", Protocols: []string{"trojan"}}, {Type: "rename", Pattern: `香港 (\d+)`, Replace: "HK-$1"}})
	if len(out) != 1 || out[0].Name != "HK-2" {
		t.Fatalf("regex rename: %+v", out)
	}
}

func TestLoonQuanx(t *testing.T) {
	for _, f := range []string{"loon", "quanx"} {
		r, _ := Get(f)
		b, err := r.Render(sample(), Info{}, "")
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(s, "Trojan") || !strings.Contains(s, "🇯🇵 日本") {
			t.Fatalf("%s:\n%s", f, s)
		}
		if f == "quanx" && !strings.Contains(s, "reality-base64-pubkey=") {
			t.Fatalf("quanx reality missing:\n%s", s)
		}
		if f == "loon" && !strings.Contains(s, "public-key=") {
			t.Fatalf("loon reality missing:\n%s", s)
		}
	}
}
