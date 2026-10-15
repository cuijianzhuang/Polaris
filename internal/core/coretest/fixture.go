// Package coretest provides shared fixtures for adapter tests.
package coretest

import (
	"encoding/json"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
)

// Users returns two deterministic-looking users.
func Users() []model.User {
	return []model.User{
		{ID: "u1", UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", Password: "pw-one-123456", SSSeed: "seed-one"},
		{ID: "u2", UUID: "a0b1c2d3-e4f5-4a6b-8c7d-9e0f1a2b3c4d", Password: "pw-two-123456", SSSeed: "seed-two"},
	}
}

// TLS returns a self-signed security spec.
func TLS(t testing.TB, sni string) model.SecuritySpec {
	c, k, err := pki.SelfSigned([]string{sni}, 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	return model.SecuritySpec{Type: model.SecTLS, ServerName: sni, CertPEM: string(c), KeyPEM: string(k)}
}

// Reality returns a REALITY security spec with fresh keys.
func Reality(t testing.TB) model.SecuritySpec {
	priv, pub, err := pki.RealityKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return model.SecuritySpec{Type: model.SecReality, RealityDest: "www.example.com:443",
		RealityServerNames: []string{"www.example.com"}, RealityPrivateKey: priv, RealityPublicKey: pub,
		RealityShortIDs: []string{pki.ShortID(8)}}
}

// Inbound builds an enabled inbound for core c.
func Inbound(c model.CoreType, tag string, port int, p model.ProtocolSpec, tr model.TransportSpec, sec model.SecuritySpec) model.Inbound {
	in := model.Inbound{ID: tag, NodeID: "n1", Tag: tag, Core: c, Port: port, Protocol: p, Transport: tr,
		Security: sec, Enabled: true, Users: Users(), Sniffing: model.SniffSpec{Enabled: true}}
	in.Normalize()
	return in
}

// Settings marshals outbound settings.
func Settings(s model.OutboundSettings) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// Chain returns outbounds and routes exercising detour, groups and rules.
func Chain() ([]model.Outbound, []model.RouteRule) {
	obs := []model.Outbound{
		{Tag: "resi", Type: model.OutSOCKS, Settings: Settings(model.OutboundSettings{Server: "203.0.113.10", Port: 1080, Username: "a", Password: "b"})},
		{Tag: "land", Type: model.OutVLESS, Settings: Settings(model.OutboundSettings{Server: "198.51.100.2", Port: 443,
			UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", Flow: "xtls-rprx-vision",
			Security: &model.ClientTLS{Type: model.SecReality, ServerName: "www.example.com",
				PublicKey: "Z84J2IelR9ch3k8VtlVhhs5ycBUlXA7wHBWcBrjqnAw", ShortID: "0123abcd"}}), Detour: "resi"},
		{Tag: "auto", Type: model.OutURLTest, Settings: Settings(model.OutboundSettings{Members: []string{"land", "resi"}})},
		{Tag: "lb", Type: model.OutBalancer, Settings: Settings(model.OutboundSettings{Members: []string{"land", "resi"},
			Strategy: model.LBLeastLoad, Fallback: "direct"})},
	}
	routes := []model.RouteRule{
		{Priority: 10, Match: model.RouteMatch{Domain: []string{"example.ai"}, Users: []string{"u1"}}, Outbound: "auto"},
		{Priority: 15, Match: model.RouteMatch{DomainSuffix: []string{"example.net"}}, Outbound: "lb"},
		{Priority: 20, Match: model.RouteMatch{IPCIDR: []string{"10.0.0.0/8"}, Port: []string{"443", "1000-2000"}}, Outbound: "block"},
		{Priority: 100, Outbound: "direct"},
	}
	return obs, routes
}
