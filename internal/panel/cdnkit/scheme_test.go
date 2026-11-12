package cdnkit

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestSchemeAndPorts(t *testing.T) {
	httpP := &store.CDNProfile{Scheme: "http", Provider: "cloudflare"}
	httpsP := &store.CDNProfile{Scheme: "https", Provider: "cloudflare"}
	legacy := &store.CDNProfile{Provider: "cloudflare"} // empty = HTTPS
	if Scheme(httpP) != "http" || Scheme(httpsP) != "https" || Scheme(legacy) != "https" {
		t.Fatal("scheme")
	}
	if got := ClientPorts(httpP); len(got) != 7 || got[0] != 80 {
		t.Fatalf("http ports %v", got)
	}
	if got := ClientPorts(httpsP); len(got) != 6 || got[0] != 443 {
		t.Fatalf("https ports %v", got)
	}
	if DefaultPort(httpP) != 80 || DefaultPort(httpsP) != 443 {
		t.Fatal("default port")
	}
	if DefaultPort(&store.CDNProfile{Scheme: "https", Ports: store.NewJSON([]int{2053})}) != 2053 {
		t.Fatal("first configured port wins")
	}
	if PortsText(httpP) == "" || PortsText(httpsP) == "" {
		t.Fatal("ports text")
	}
}

func TestOriginTLS(t *testing.T) {
	full := &store.CDNProfile{Scheme: "https", Provider: "cloudflare", TLSMode: "full"}
	flex := &store.CDNProfile{Scheme: "https", Provider: "cloudflare", TLSMode: "flexible"}
	if !OriginTLS(full) || !OriginTLSAt(full, 2053) {
		t.Fatal("full uses TLS")
	}
	if OriginTLS(flex) {
		t.Fatal("flexible on :443 is plain HTTP")
	}
	if !OriginTLSAt(flex, 2053) {
		t.Fatal("flexible off :443 falls back to Full")
	}
	if OriginTLS(&store.CDNProfile{Scheme: "http", Provider: "cloudflare", TLSMode: "full"}) {
		t.Fatal("http never uses origin TLS")
	}
	if OriginPort(flex, 443) != 80 || OriginPort(flex, 2053) != 2053 || OriginPort(full, 443) != 443 {
		t.Fatal("origin port mapping")
	}
	tun := &store.CDNProfile{Scheme: "https", Provider: "cloudflare", TLSMode: "full", OriginMode: "tunnel"}
	if OriginTLS(tun) {
		t.Fatal("tunnel never uses origin TLS")
	}
	if !Tunnel(tun) || Tunnel(full) {
		t.Fatal("tunnel detection")
	}
}

func TestValidTunnelID(t *testing.T) {
	if !ValidTunnelID("123e4567-e89b-12d3-a456-426614174000") {
		t.Fatal("valid uuid rejected")
	}
	for _, bad := range []string{"", "not-a-uuid", "123E4567-E89B-12D3-A456-426614174000"} {
		if ValidTunnelID(bad) {
			t.Fatalf("invalid uuid accepted: %q", bad)
		}
	}
}

func TestTunnelProblem(t *testing.T) {
	ws := &model.Inbound{Listen: "127.0.0.1", Transport: model.TransportSpec{Type: model.TransWS}, Security: model.SecuritySpec{Type: model.SecNone}}
	good := &store.CDNProfile{Provider: "cloudflare", Scheme: "https", TunnelID: "123e4567-e89b-12d3-a456-426614174000", OriginMode: "tunnel", Ports: store.NewJSON([]int{443})}
	if got := TunnelProblem(ws, good, 443); got != "" {
		t.Fatalf("good tunnel: %s", got)
	}
	if got := TunnelProblem(ws, good, 2053); got == "" {
		t.Fatal("tunnel on non-443 accepted")
	}
	bad := *good
	bad.TunnelID = "nope"
	if got := TunnelProblem(ws, &bad, 443); got == "" {
		t.Fatal("bad tunnel id accepted")
	}
	tls := *ws
	tls.Security.Type = model.SecTLS
	if got := TunnelProblem(&tls, good, 443); got == "" {
		t.Fatal("tunnel with TLS inbound accepted")
	}
}

func TestNodePort(t *testing.T) {
	httpsP := &store.CDNProfile{Scheme: "https", Provider: "cloudflare"}
	if NodePort(httpsP, 2053, false) != 2053 || NodePort(httpsP, 10086, false) != 443 || NodePort(httpsP, 2053, true) != 443 {
		t.Fatal("node port")
	}
	if NodePort(&store.CDNProfile{Scheme: "https", OriginMode: "tunnel"}, 2053, false) != 443 {
		t.Fatal("tunnel node port is 443")
	}
}

func TestProblem(t *testing.T) {
	ws := func(port int) *model.Inbound {
		return &model.Inbound{Port: port, Transport: model.TransportSpec{Type: model.TransWS}, Security: model.SecuritySpec{Type: model.SecNone}}
	}
	tls := func(port int) *model.Inbound {
		return &model.Inbound{Port: port, Transport: model.TransportSpec{Type: model.TransWS}, Security: model.SecuritySpec{Type: model.SecTLS}}
	}
	httpsP := &store.CDNProfile{Scheme: "https", Provider: "cloudflare", TLSMode: "full"}
	if got := Problem(tls(2053), httpsP, 0, false); got != "" {
		t.Fatalf("tls on 2053: %s", got)
	}
	// Plain inbound cannot take a Full origin: the origin would dial TLS.
	if got := Problem(ws(443), httpsP, 0, false); got == "" {
		t.Fatal("plain inbound on Full accepted")
	}
	if got := Problem(tls(443), httpsP, 0, false); got != "" {
		t.Fatalf("tls inbound on Full: %s", got)
	}
	// Inbound port is not a Cloudflare edge port and cannot be mapped.
	if got := Problem(ws(10086), httpsP, 0, false); !strings.Contains(got, "不能自动映射") {
		t.Fatalf("unmapped port: %q", got)
	}
	httpP := &store.CDNProfile{Scheme: "http", Provider: "cloudflare"}
	if got := Problem(ws(8080), httpP, 0, true); got == "" {
		t.Fatal("HTTP with Caddy fronting accepted")
	}
	grpc := &model.Inbound{Port: 80, Transport: model.TransportSpec{Type: model.TransGRPC}, Security: model.SecuritySpec{Type: model.SecNone}}
	if got := Problem(grpc, httpP, 0, false); got == "" {
		t.Fatal("gRPC over HTTP accepted")
	}
	other := &store.CDNProfile{Scheme: "https", Provider: "other", TLSMode: "full"}
	if got := Problem(ws(10086), other, 0, false); got != "" {
		t.Fatalf("non-cloudflare restricted: %s", got)
	}
}
