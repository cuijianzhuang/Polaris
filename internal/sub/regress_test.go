package sub

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestShareLinkEscapesCredentials(t *testing.T) {
	for _, proto := range []model.Protocol{model.ProtoTrojan, model.ProtoHysteria2, model.ProtoAnyTLS} {
		n := Node{Name: "x", Server: "1.2.3.4", Port: 443, Protocol: proto, Password: "p:a@ss/1 +%",
			TLS: model.ClientTLS{Type: model.SecTLS, ServerName: "a.example"}}
		got, err := ParseLink(ShareLink(n))
		if err != nil {
			t.Fatalf("%s: %v", proto, err)
		}
		if got.Password != n.Password {
			t.Errorf("%s: password round trip = %q, want %q (%s)", proto, got.Password, n.Password, ShareLink(n))
		}
	}
}

func TestDedupeNeverDuplicates(t *testing.T) {
	in := []Node{{Name: "HK 2"}, {Name: "HK"}, {Name: "HK"}, {Name: "HK"}, {Name: "HK 3"}}
	seen := map[string]bool{}
	for _, n := range Dedupe(in) {
		if seen[n.Name] {
			t.Fatalf("duplicate name %q in %v", n.Name, Dedupe(in))
		}
		seen[n.Name] = true
	}
}

func TestLoonQuanxSkipUnsupportedTrojanTransport(t *testing.T) {
	n := Node{Name: "g", Server: "1.2.3.4", Port: 443, Protocol: model.ProtoTrojan, Password: "pw",
		Transport: model.TransportSpec{Type: model.TransGRPC, ServiceName: "svc"}, TLS: model.ClientTLS{Type: model.SecTLS}}
	if l := LoonLine(n); l != "" {
		t.Errorf("Loon emitted a gRPC trojan node as plain TCP: %s", l)
	}
	if l := QuanxLine(n); l != "" {
		t.Errorf("Quantumult X emitted a gRPC trojan node as plain TCP: %s", l)
	}
}

func TestQuanxBracketsIPv6(t *testing.T) {
	n := Node{Name: "v6", Server: "2001:db8::1", Port: 443, Protocol: model.ProtoShadowsocks, Method: "aes-128-gcm", Password: "pw"}
	if l := QuanxLine(n); !strings.Contains(l, "[2001:db8::1]:443") {
		t.Errorf("IPv6 server not bracketed: %s", l)
	}
}

func TestVLESSEncryptionNodes(t *testing.T) {
	n := Node{Name: "pq", Server: "1.2.3.4", Port: 443, Protocol: model.ProtoVLESS, UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01",
		Flow: "xtls-rprx-vision", Encryption: "mlkem768x25519plus.native.0rtt.abc",
		TLS: model.ClientTLS{Type: model.SecReality, ServerName: "a.example", PublicKey: "pbk", ShortID: "ab", SpiderX: "/x", Mldsa65Verify: "pqvkey"}}
	got, err := ParseLink(ShareLink(n))
	if err != nil || got.Encryption != n.Encryption || got.TLS.Mldsa65Verify != "pqvkey" || got.TLS.SpiderX != "/x" {
		t.Fatalf("round trip: %+v %v (%s)", got, err, ShareLink(n))
	}
	if SingboxOutbound(n) != nil || LoonLine(n) != "" || QuanxLine(n) != "" {
		t.Fatal("clients without VLESS Encryption must skip the node")
	}
	if p := ClashProxy(n); p == nil || p["encryption"] != n.Encryption {
		t.Fatalf("mihomo proxy: %v", p)
	}
}
