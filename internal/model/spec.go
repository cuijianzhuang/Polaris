package model

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sort"
	"time"
)

// User is a panel user as seen by a core. ID doubles as the core-side
// email/name so traffic from every core maps back to the same person.
type User struct {
	ID       string `json:"id"`
	UUID     string `json:"uuid"`
	Password string `json:"password"`
	// SSSeed is a per-user random secret; SS 2022 PSKs are derived from it so
	// that every cipher gets a key of the right length.
	SSSeed string `json:"ssSeed,omitempty"`
	Level  int    `json:"level,omitempty"`
}

// SSKey derives the user's Shadowsocks key for the given method. 2022 ciphers
// need a base64 PSK of exactly the cipher's key length; legacy ciphers take the
// password.
func (u User) SSKey(method string) string {
	n := SS2022KeyLen(method)
	if n == 0 {
		return u.Password
	}
	m := hmac.New(sha256.New, []byte(u.SSSeed))
	m.Write([]byte(method))
	return base64.StdEncoding.EncodeToString(m.Sum(nil)[:n])
}

// SS2022KeyLen returns the PSK length for a 2022 cipher, or 0 for others.
func SS2022KeyLen(method string) int {
	switch method {
	case "2022-blake3-aes-128-gcm":
		return 16
	case "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
		return 32
	}
	return 0
}

// CoreSpec says which core (and version) should run on a node.
type CoreSpec struct {
	Type    CoreType `json:"type"`
	Version string   `json:"version"`
	SHA256  string   `json:"sha256,omitempty"` // expected archive digest, if known
}

// Forwarding engines.
const (
	EngineRealm = "" // realm (the forwarder core)
	EngineGost  = "gost"
)

// GostForwards / RealmForwards split a spec's forwards by engine.
func (s *NodeSpec) GostForwards() []ForwardRule  { return s.forwardsBy(true) }
func (s *NodeSpec) RealmForwards() []ForwardRule { return s.forwardsBy(false) }

func (s *NodeSpec) forwardsBy(gost bool) []ForwardRule {
	var out []ForwardRule
	for _, f := range s.Forwards {
		if (f.Engine == EngineGost) == gost {
			out = append(out, f)
		}
	}
	return out
}

// ListenNetwork is what the rule's listening socket uses: a GOST tunnel end
// over QUIC / KCP listens on UDP whatever it carries, a relay tunnel end
// over a stream transport on TCP.
func (f *ForwardRule) ListenNetwork() string {
	if f.Engine == EngineGost && f.TunnelRole == "server" {
		if f.TunnelType == "quic" || f.TunnelType == "kcp" {
			return "udp"
		}
		return "tcp"
	}
	if f.Network == "" {
		return "tcp"
	}
	return f.Network
}

// ForwardRule is an L4 port forward run by realm (the forwarder core) or GOST.
type ForwardRule struct {
	Tag      string `json:"tag"`
	Engine   string `json:"engine,omitempty"`   // "" (realm) / gost
	ListenIP string `json:"listenIp,omitempty"` // "" = all addresses
	Listen   int    `json:"listen"`
	Network  string `json:"network"` // tcp / udp / tcp,udp
	Target   string `json:"target"`  // host:port
	// ExtraTargets are further remotes; Balance spreads connections over all
	// of them ("roundrobin" / "iphash", Weights one per remote, Target first).
	ExtraTargets []string `json:"extraTargets,omitempty"`
	Balance      string   `json:"balance,omitempty"`
	Weights      []int    `json:"weights,omitempty"`
	Through      string   `json:"through,omitempty"` // outgoing source IP
	// realm transports between a relay and a landing realm, e.g.
	// "ws;host=a;path=/x;tls;sni=a;insecure" / "ws;host=a;path=/x;tls;servername=a".
	ListenTransport string `json:"listenTransport,omitempty"`
	RemoteTransport string `json:"remoteTransport,omitempty"`
	SendProxy       int    `json:"sendProxy,omitempty"` // PROXY protocol version sent to the remote (0 = off)
	AcceptProxy     bool   `json:"acceptProxy,omitempty"`

	// GOST: native failover, relay tunnels (TunnelRole client / server over
	// TunnelType ws / wss / tls / mwss / mtls / kcp / quic / grpc, with an
	// optional relay password) and limits (KB/s, concurrent connections).
	Failover   bool   `json:"failover,omitempty"`
	TunnelRole string `json:"tunnelRole,omitempty"`
	TunnelType string `json:"tunnelType,omitempty"`
	TunnelHost string `json:"tunnelHost,omitempty"`
	TunnelPath string `json:"tunnelPath,omitempty"`
	TunnelSNI  string `json:"tunnelSni,omitempty"`
	TunnelAuth string `json:"tunnelAuth,omitempty"`
	RateIn     int    `json:"rateIn,omitempty"`
	RateOut    int    `json:"rateOut,omitempty"`
	MaxConns   int    `json:"maxConns,omitempty"`
}

// FrontRule makes the node's Caddy terminate TLS for Domain on Port and
// reverse-proxy Path to a loopback inbound (CDN inbounds sharing 443).
type FrontRule struct {
	Domain   string    `json:"domain"`
	Port     int       `json:"port"`
	Path     string    `json:"path"`     // ws / httpupgrade / xhttp path, or "/<serviceName>" for gRPC
	Upstream string    `json:"upstream"` // 127.0.0.1:port
	H2C      bool      `json:"h2c"`      // gRPC / xhttp upstreams speak h2c
	CertPEM  string    `json:"certPem"`
	KeyPEM   string    `json:"keyPem"`
	Inbound  string    `json:"inbound"`
	Kind     Transport `json:"kind"`
}

// NodeSpec is the complete desired state of one node at one revision. The
// panel stores every revision; the agent converges the host onto it.
type NodeSpec struct {
	NodeID      string        `json:"nodeId"`
	Rev         int64         `json:"rev"`
	DefaultCore CoreType      `json:"defaultCore"`
	Cores       []CoreSpec    `json:"cores"`
	Inbounds    []Inbound     `json:"inbounds"`
	Outbounds   []Outbound    `json:"outbounds,omitempty"`
	Routes      []RouteRule   `json:"routes,omitempty"`
	Forwards    []ForwardRule `json:"forwards,omitempty"`
	Fronts      []FrontRule   `json:"fronts,omitempty"`
	// IPLimits maps user id -> max concurrent source IPs (0 = unlimited);
	// the agent uses it to raise alerts.
	IPLimits map[string]int `json:"ipLimits,omitempty"`
	// AutoFirewall lets the agent open the spec's ports in the host
	// firewall (ufw / firewalld / iptables) and close them again.
	AutoFirewall bool `json:"autoFirewall,omitempty"`
	// SpeedLimits caps the bandwidth of inbound ports with tc on the node;
	// everyone on the port shares the cap.
	SpeedLimits []SpeedLimit `json:"speedLimits,omitempty"`
	// Geodata makes Xray download its .dat files on a schedule and reload
	// them without a restart (Xray 26.4.25+).
	Geodata   *GeodataSpec `json:"geodata,omitempty"`
	CreatedAt time.Time    `json:"createdAt"`
}

// SpeedLimit is one port's bandwidth cap, each direction.
type SpeedLimit struct {
	Tag     string `json:"tag"`
	Port    int    `json:"port"`
	Network string `json:"network"` // "tcp", "udp" or "tcp,udp"
	Mbps    int    `json:"mbps"`
}

// GeodataSpec is Xray's "geodata" block.
type GeodataSpec struct {
	Cron     string     `json:"cron"`
	Outbound string     `json:"outbound,omitempty"` // download through this outbound tag
	Assets   []GeoAsset `json:"assets"`
}

// GeoAsset is one .dat file and where to fetch it (https only).
type GeoAsset struct {
	URL  string `json:"url"`
	File string `json:"file"`
}

// StandardGeoAssets are the commonly used rule sets.
var StandardGeoAssets = []GeoAsset{
	{URL: "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat", File: "geoip.dat"},
	{URL: "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat", File: "geosite.dat"},
}

// InboundsFor returns the enabled inbounds hosted by the given core.
func (s *NodeSpec) InboundsFor(c CoreType) []Inbound {
	var out []Inbound
	for _, in := range s.Inbounds {
		if in.Core == c && in.Enabled {
			out = append(out, in)
		}
	}
	return out
}

// HasCore reports whether the spec asks for the given core.
func (s *NodeSpec) HasCore(c CoreType) bool {
	for _, cs := range s.Cores {
		if cs.Type == c {
			return true
		}
	}
	return false
}

// TrafficDelta is a traffic increment for one user since the last collection.
type TrafficDelta struct {
	UserID string `json:"userId"`
	Up     int64  `json:"up"`
	Down   int64  `json:"down"`
}

// MergeDeltas sums deltas per user.
func MergeDeltas(ds ...[]TrafficDelta) []TrafficDelta {
	m := map[string]*TrafficDelta{}
	for _, d := range ds {
		for _, t := range d {
			if t.Up == 0 && t.Down == 0 {
				continue
			}
			cur, ok := m[t.UserID]
			if !ok {
				cur = &TrafficDelta{UserID: t.UserID}
				m[t.UserID] = cur
			}
			cur.Up += t.Up
			cur.Down += t.Down
		}
	}
	out := make([]TrafficDelta, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out
}

// CoreStatus is the health snapshot of one core process.
type CoreStatus struct {
	Type      CoreType  `json:"type"`
	Version   string    `json:"version"`
	Running   bool      `json:"running"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	Error     string    `json:"error,omitempty"`
	Inbounds  int       `json:"inbounds"`
}

// Clone deep-copies a spec via JSON.
func (s *NodeSpec) Clone() *NodeSpec {
	b, _ := json.Marshal(s)
	var out NodeSpec
	_ = json.Unmarshal(b, &out)
	return &out
}
