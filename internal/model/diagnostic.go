package model

import (
	"strings"
	"time"
)

const RelayBusinessFeature = "relay-business-v1"
const SingboxRelayBusinessFeature = "relay-business-singbox-v1"
const SingboxRelayBusinessMinVersion = "1.13.5"
const DiagnosticUserPrefix = "#diag:"

func IsDiagnosticCounter(id string) bool { return strings.HasPrefix(id, DiagnosticUserPrefix) }

type RelayBusinessRequest struct {
	Core         CoreType `json:"core,omitempty"`
	ID           string   `json:"id"`
	ExpiresAt    int64    `json:"expiresAt"`
	InboundTag   string   `json:"inboundTag"`
	EntryAddress string   `json:"entryAddress"`
	EntryPort    int      `json:"entryPort"`
	ServerName   string   `json:"serverName,omitempty"`
	TCPURL       string   `json:"tcpUrl"`
	UDPAddress   string   `json:"udpAddress"`
}

type BusinessCheck struct {
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	LatencyMs int64  `json:"latencyMs,omitempty"`
	UpBytes   int64  `json:"upBytes,omitempty"`
	DownBytes int64  `json:"downBytes,omitempty"`
}

// Keep the initial sing-box diagnostic surface deliberately narrow. Inbound
// JSON overrides and shared fallback listeners need their own validation.
func SingboxRelayBusinessInbound(in Inbound) bool {
	in.Normalize()
	extra := strings.TrimSpace(string(in.Extra))
	return in.Core == CoreSingbox && in.Protocol.Type == ProtoVLESS &&
		in.Transport.Type == TransTCP && in.Security.Type == SecReality &&
		in.Protocol.Flow == "xtls-rprx-vision" && (extra == "" || extra == "null" || extra == "{}") &&
		len(in.Protocol.Fallbacks) == 0 && !in.Transport.AcceptProxyProtocol
}

type RelayBusinessResult struct {
	CheckedAt        time.Time       `json:"checkedAt"`
	State            string          `json:"state"`
	BusinessVerified bool            `json:"businessVerified"`
	Checks           []BusinessCheck `json:"checks"`
	CleanupVerified  bool            `json:"cleanupVerified"`
}
