package model

import "encoding/json"

// OutboundType enumerates supported outbound kinds.
type OutboundType string

const (
	OutDirect      OutboundType = "direct"
	OutBlock       OutboundType = "block"
	OutSOCKS       OutboundType = "socks"
	OutHTTP        OutboundType = "http"
	OutShadowsocks OutboundType = "shadowsocks"
	OutVLESS       OutboundType = "vless"
	OutVMess       OutboundType = "vmess"
	OutTrojan      OutboundType = "trojan"
	OutHysteria2   OutboundType = "hysteria2"
	OutWireGuard   OutboundType = "wireguard"
	OutNodeRef     OutboundType = "node_ref" // resolved by the panel before reaching a core
	OutURLTest     OutboundType = "urltest"  // auto-select group (xray: balancer + observatory)
	OutSelector    OutboundType = "selector" // manual group
	OutBalancer    OutboundType = "balancer" // load balancing group with a strategy
)

// Load-balancing strategies for OutBalancer.
const (
	LBLeastPing  = "leastPing"  // lowest latency (Xray observatory / sing-box urltest)
	LBLeastLoad  = "leastLoad"  // lowest latency jitter + load (Xray burstObservatory)
	LBRandom     = "random"     // random per connection (Xray only)
	LBRoundRobin = "roundRobin" // rotate per connection (Xray only)
	LBFallback   = "fallback"   // first healthy member in order
)

// GlobalNodeID marks a routing rule that applies to every node.
const GlobalNodeID = "*"

// InboundRef points at an inbound on another node managed by this panel.
type InboundRef struct {
	InboundID string `json:"inboundId"`
}

// Outbound is a node egress. Detour chains outbounds into a multi-hop path.
type Outbound struct {
	ID       string          `json:"id"`
	NodeID   string          `json:"nodeId"`
	Tag      string          `json:"tag"`
	Type     OutboundType    `json:"type"`
	Settings json.RawMessage `json:"settings,omitempty"`
	NodeRef  *InboundRef     `json:"nodeRef,omitempty"`
	Detour   string          `json:"detour,omitempty"`
}

// OutboundSettings is the typed view of Outbound.Settings.
type OutboundSettings struct {
	Server   string `json:"server,omitempty"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	UUID     string `json:"uuid,omitempty"`
	Flow     string `json:"flow,omitempty"`
	Method   string `json:"method,omitempty"`
	// Encryption is a VLESS client's "encryption" ("" = none).
	Encryption string `json:"encryption,omitempty"`

	Transport *TransportSpec `json:"transport,omitempty"`
	Security  *ClientTLS     `json:"security,omitempty"`

	// hysteria2
	UpMbps       int    `json:"upMbps,omitempty"`
	DownMbps     int    `json:"downMbps,omitempty"`
	ObfsPassword string `json:"obfsPassword,omitempty"`

	// wireguard
	PrivateKey    string   `json:"privateKey,omitempty"`
	PeerPublicKey string   `json:"peerPublicKey,omitempty"`
	PreSharedKey  string   `json:"preSharedKey,omitempty"`
	LocalAddress  []string `json:"localAddress,omitempty"`
	Reserved      []int    `json:"reserved,omitempty"`
	MTU           int      `json:"mtu,omitempty"`

	// groups
	Members   []string `json:"members,omitempty"`
	TestURL   string   `json:"testUrl,omitempty"`
	Interval  string   `json:"interval,omitempty"`
	Strategy  string   `json:"strategy,omitempty"`  // balancer strategy
	Tolerance int      `json:"tolerance,omitempty"` // ms, latency difference tolerated before switching
	Fallback  string   `json:"fallback,omitempty"`  // tag used when every member is down
}

// IsGroup reports outbound types that select among member outbounds.
func (t OutboundType) IsGroup() bool {
	return t == OutURLTest || t == OutSelector || t == OutBalancer
}

// ClientTLS is the client-side view of a SecuritySpec.
type ClientTLS struct {
	Type        Security `json:"type"`
	ServerName  string   `json:"serverName,omitempty"`
	ALPN        []string `json:"alpn,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	Insecure    bool     `json:"insecure,omitempty"`
	PublicKey   string   `json:"publicKey,omitempty"` // reality
	ShortID     string   `json:"shortId,omitempty"`   // reality
	SpiderX     string   `json:"spiderX,omitempty"`   // reality
	// Mldsa65Verify is the REALITY server's ML-DSA-65 public key: the
	// client then checks the server's post-quantum signature.
	Mldsa65Verify string `json:"mldsa65Verify,omitempty"`
}

// ParseSettings decodes Settings into the typed view.
func (o *Outbound) ParseSettings() (OutboundSettings, error) {
	var s OutboundSettings
	if len(o.Settings) == 0 {
		return s, nil
	}
	err := json.Unmarshal(o.Settings, &s)
	return s, err
}

// RouteMatch is the condition part of a routing rule. All set fields must match.
type RouteMatch struct {
	Inbound       []string `json:"inbound,omitempty"`
	UserGroups    []string `json:"userGroups,omitempty"` // resolved into Users by the panel
	Users         []string `json:"users,omitempty"`
	Domain        []string `json:"domain,omitempty"`
	DomainSuffix  []string `json:"domainSuffix,omitempty"`
	DomainKeyword []string `json:"domainKeyword,omitempty"`
	Geosite       []string `json:"geosite,omitempty"`
	GeoIP         []string `json:"geoip,omitempty"`
	IPCIDR        []string `json:"ipCidr,omitempty"`
	// RuleSets names panel rule sets; the panel expands them into the
	// fields above before the spec reaches a node.
	RuleSets []string `json:"ruleSets,omitempty"`
	Port     []string `json:"port,omitempty"` // "443" or "1000-2000"
	Network  string   `json:"network,omitempty"`
}

// RouteRule sends matching traffic to an outbound (or group) tag.
type RouteRule struct {
	ID       string     `json:"id"`
	NodeID   string     `json:"nodeId"`
	Match    RouteMatch `json:"match"`
	Outbound string     `json:"outbound"`
	Priority int        `json:"priority"`
}
