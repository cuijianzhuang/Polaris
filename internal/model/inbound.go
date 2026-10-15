// Package model holds Polaris' unified, core-agnostic data model. The panel
// only ever reasons about these types; every core adapter translates them into
// its own native configuration.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CoreType identifies a proxy core implementation.
type CoreType string

const (
	CoreXray      CoreType = "xray"
	CoreSingbox   CoreType = "singbox"
	CoreMihomo    CoreType = "mihomo"
	CoreHysteria  CoreType = "hysteria"
	CoreForwarder CoreType = "forwarder" // L4 port forwarder (nftables / realm)
	CoreCaddy     CoreType = "caddy"     // TLS front for CDN inbounds sharing one port
	CoreAuto      CoreType = "auto"      // templates only: pick by capability
)

// Protocol is the inbound proxy protocol.
type Protocol string

const (
	ProtoVLESS       Protocol = "vless"
	ProtoVMess       Protocol = "vmess"
	ProtoTrojan      Protocol = "trojan"
	ProtoShadowsocks Protocol = "shadowsocks"
	ProtoHysteria2   Protocol = "hysteria2"
	ProtoTUIC        Protocol = "tuic"
	ProtoAnyTLS      Protocol = "anytls"
	ProtoShadowTLS   Protocol = "shadowtls"
	ProtoNaive       Protocol = "naive"
	ProtoSOCKS       Protocol = "socks"
	ProtoHTTP        Protocol = "http"
)

// AllProtocols lists every protocol known to the model, in display order.
var AllProtocols = []Protocol{
	ProtoVLESS, ProtoVMess, ProtoTrojan, ProtoShadowsocks, ProtoHysteria2,
	ProtoTUIC, ProtoAnyTLS, ProtoShadowTLS, ProtoNaive, ProtoSOCKS, ProtoHTTP,
}

// Transport is the stream transport below the protocol.
type Transport string

const (
	TransTCP         Transport = "tcp"
	TransWS          Transport = "ws"
	TransGRPC        Transport = "grpc"
	TransHTTPUpgrade Transport = "httpupgrade"
	TransXHTTP       Transport = "xhttp"
	TransKCP         Transport = "kcp"
	TransQUIC        Transport = "quic"
)

var AllTransports = []Transport{TransTCP, TransWS, TransGRPC, TransHTTPUpgrade, TransXHTTP, TransKCP, TransQUIC}

// Security is the transport security layer.
type Security string

const (
	SecNone    Security = "none"
	SecTLS     Security = "tls"
	SecReality Security = "reality"
)

var AllSecurities = []Security{SecNone, SecTLS, SecReality}

// ProtocolSpec carries protocol-level settings. Only the fields relevant to
// Type are used.
type ProtocolSpec struct {
	Type Protocol `json:"type"`

	Flow string `json:"flow,omitempty"` // vless: "xtls-rprx-vision"

	Method    string `json:"method,omitempty"`    // shadowsocks cipher, e.g. 2022-blake3-aes-128-gcm
	ServerKey string `json:"serverKey,omitempty"` // shadowsocks 2022 server PSK (encrypted at rest)

	CongestionControl string `json:"congestionControl,omitempty"` // tuic: bbr / cubic / new_reno

	UpMbps       int    `json:"upMbps,omitempty"` // hysteria2 bandwidth hints
	DownMbps     int    `json:"downMbps,omitempty"`
	Obfs         string `json:"obfs,omitempty"` // hysteria2: "salamander"
	ObfsPassword string `json:"obfsPassword,omitempty"`

	HandshakeServer string `json:"handshakeServer,omitempty"` // shadowtls: host:port of the real TLS server
	Detour          string `json:"detour,omitempty"`          // shadowtls: tag of the inner inbound

	UDP bool `json:"udp,omitempty"` // socks
}

// TransportSpec carries transport-level settings.
type TransportSpec struct {
	Type        Transport `json:"type"`
	Path        string    `json:"path,omitempty"`        // ws / httpupgrade / xhttp
	Host        string    `json:"host,omitempty"`        // ws / httpupgrade / xhttp
	ServiceName string    `json:"serviceName,omitempty"` // grpc
	Mode        string    `json:"mode,omitempty"`        // xhttp: auto / packet-up / stream-up / stream-one
	HeaderType  string    `json:"headerType,omitempty"`  // kcp / tcp header obfuscation
	Seed        string    `json:"seed,omitempty"`        // kcp
}

// SecuritySpec carries TLS / REALITY settings.
type SecuritySpec struct {
	Type        Security `json:"type"`
	ServerName  string   `json:"serverName,omitempty"`
	ALPN        []string `json:"alpn,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"` // client uTLS fingerprint
	Insecure    bool     `json:"insecure,omitempty"`    // client: skip verify (self-signed)

	CertificateID string `json:"certificateId,omitempty"` // reference into the certificates table
	CertPEM       string `json:"certPem,omitempty"`       // resolved when building a NodeSpec
	KeyPEM        string `json:"keyPem,omitempty"`

	RealityDest        string   `json:"realityDest,omitempty"` // host:port of the camouflage site
	RealityServerNames []string `json:"realityServerNames,omitempty"`
	RealityPrivateKey  string   `json:"realityPrivateKey,omitempty"` // encrypted at rest
	RealityPublicKey   string   `json:"realityPublicKey,omitempty"`
	RealityShortIDs    []string `json:"realityShortIds,omitempty"`
}

// SniffSpec controls traffic sniffing.
type SniffSpec struct {
	Enabled      bool     `json:"enabled"`
	DestOverride []string `json:"destOverride,omitempty"`
	RouteOnly    bool     `json:"routeOnly,omitempty"`
}

// Inbound is one listener on one node, owned by exactly one core.
type Inbound struct {
	ID        string          `json:"id"`
	NodeID    string          `json:"nodeId"`
	Tag       string          `json:"tag"`
	Core      CoreType        `json:"core"`
	Listen    string          `json:"listen,omitempty"`
	Port      int             `json:"port"`
	Protocol  ProtocolSpec    `json:"protocol"`
	Transport TransportSpec   `json:"transport"`
	Security  SecuritySpec    `json:"security"`
	Sniffing  SniffSpec       `json:"sniffing"`
	Extra     json.RawMessage `json:"extra,omitempty"` // escape hatch: merged verbatim into the native config
	Enabled   bool            `json:"enabled"`

	// Users is populated only inside a NodeSpec: the users authorised on this
	// inbound at this revision.
	Users []User `json:"users,omitempty"`
}

// Network returns the L4 network(s) the inbound listens on.
func (in *Inbound) Network() string {
	switch in.Protocol.Type {
	case ProtoHysteria2, ProtoTUIC:
		return "udp"
	case ProtoShadowsocks:
		return "tcp,udp"
	}
	if in.Transport.Type == TransKCP || in.Transport.Type == TransQUIC {
		return "udp"
	}
	return "tcp"
}

// UsesUDP / UsesTCP report which L4 ports the inbound occupies, for conflict checks.
func (in *Inbound) UsesUDP() bool { return strings.Contains(in.Network(), "udp") }
func (in *Inbound) UsesTCP() bool { return strings.Contains(in.Network(), "tcp") }

// Normalize fills implicit defaults so that equivalent inbounds compare equal.
func (in *Inbound) Normalize() {
	if in.Transport.Type == "" {
		in.Transport.Type = TransTCP
	}
	if in.Security.Type == "" {
		in.Security.Type = SecNone
	}
	switch in.Protocol.Type {
	case ProtoHysteria2, ProtoTUIC:
		in.Transport.Type = TransQUIC
		if in.Security.Type == SecNone {
			in.Security.Type = SecTLS
		}
	case ProtoAnyTLS, ProtoNaive:
		if in.Security.Type == SecNone {
			in.Security.Type = SecTLS
		}
	}
	if in.Listen == "" {
		in.Listen = "::"
	}
}

// Validate checks structural consistency that does not depend on a core.
func (in *Inbound) Validate() error {
	if in.Tag == "" {
		return fmt.Errorf("tag is required")
	}
	if in.Port <= 0 || in.Port > 65535 {
		return fmt.Errorf("port %d out of range", in.Port)
	}
	known := false
	for _, p := range AllProtocols {
		if p == in.Protocol.Type {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("unknown protocol %q", in.Protocol.Type)
	}
	if in.Security.Type == SecReality {
		if in.Security.RealityDest == "" || len(in.Security.RealityServerNames) == 0 {
			return fmt.Errorf("reality requires dest and serverNames")
		}
	}
	if in.Protocol.Type == ProtoShadowsocks && in.Protocol.Method == "" {
		return fmt.Errorf("shadowsocks requires method")
	}
	if len(in.Extra) > 0 && !json.Valid(in.Extra) {
		return fmt.Errorf("extra is not valid JSON")
	}
	return nil
}

// CDNCapable reports whether clients may reach this inbound through a CDN.
func (in *Inbound) CDNCapable() bool {
	if in.Security.Type == SecReality {
		return false
	}
	switch in.Transport.Type {
	case TransWS, TransHTTPUpgrade, TransGRPC, TransXHTTP:
		return true
	}
	return false
}
