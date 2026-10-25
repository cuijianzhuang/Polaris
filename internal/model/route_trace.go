package model

import (
	"errors"
	"net/netip"
	"time"
)

const RouteTraceFeature = "route-trace-v1"
const RouteTraceToolFeature = "route-trace-tool-v1"

// Allow all 20 one-second hops to complete, including implementations that
// send probes sequentially. The panel reserves extra time for command delivery.
const RouteTraceProbeTimeout = 24 * time.Second
const RouteTraceRequestTimeout = RouteTraceProbeTimeout + 4*time.Second
const RouteTraceStaleAfter = 45 * time.Second

// Tool readiness is independent of network reachability. Ready only means the
// executable can perform a local UDP probe with our bounded arguments.
type RouteTraceToolStatus struct {
	State         string    `json:"state"`
	Issue         string    `json:"issue,omitempty"`
	Path          string    `json:"path,omitempty"`
	CheckOutput   string    `json:"checkOutput,omitempty"`
	Manager       string    `json:"manager,omitempty"`
	CanInstall    bool      `json:"canInstall"`
	ManualCommand string    `json:"manualCommand,omitempty"`
	CheckedAt     time.Time `json:"checkedAt"`
	InstallState  string    `json:"installState,omitempty"`
	InstallOutput string    `json:"installOutput,omitempty"`
	InstallIssue  string    `json:"installIssue,omitempty"`
}

type RouteTraceRequest struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// Literal targets avoid DNS changing address families or resolving a public
// name to the Agent's local network after validation.
func (r RouteTraceRequest) Validate() error {
	if r.IPv4 == "" && r.IPv6 == "" {
		return errors.New("至少指定一个公网 IPv4 或 IPv6 地址")
	}
	for _, target := range []struct {
		value string
		v4    bool
	}{{r.IPv4, true}, {r.IPv6, false}} {
		if target.value == "" {
			continue
		}
		ip, err := netip.ParseAddr(target.value)
		if err != nil || ip.Zone() != "" || ip.Is4In6() || ip.Is4() != target.v4 || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return errors.New("测试目标必须为对应地址族的公网 IP 字面量")
		}
	}
	return nil
}

type RouteTraceHop struct {
	Geo       []RouteTraceIPGeo `json:"geo,omitempty"`
	ASN       []RouteTraceIPASN `json:"asn,omitempty"`
	TTL       int               `json:"ttl"`
	Addresses []string          `json:"addresses"`
}

type RouteTraceResult struct {
	Geo       *RouteTraceGeoMetadata `json:"geo,omitempty"`
	ASN       *RouteTraceASNMetadata `json:"asn,omitempty"`
	Family    string                 `json:"family"`
	Target    string                 `json:"target"`
	CheckedAt time.Time              `json:"checkedAt"`
	State     string                 `json:"state"` // reached / partial / timeout / unavailable / failed
	Issue     string                 `json:"issue,omitempty"`
	Hops      []RouteTraceHop        `json:"hops"`
	Raw       string                 `json:"raw"`
}
