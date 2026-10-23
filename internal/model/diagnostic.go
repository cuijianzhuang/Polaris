package model

import (
	"strings"
	"time"
)

const RelayBusinessFeature = "relay-business-v1"
const DiagnosticUserPrefix = "#diag:"

func IsDiagnosticCounter(id string) bool { return strings.HasPrefix(id, DiagnosticUserPrefix) }

type RelayBusinessRequest struct {
	ID           string `json:"id"`
	ExpiresAt    int64  `json:"expiresAt"`
	InboundTag   string `json:"inboundTag"`
	EntryAddress string `json:"entryAddress"`
	EntryPort    int    `json:"entryPort"`
	ServerName   string `json:"serverName,omitempty"`
	TCPURL       string `json:"tcpUrl"`
	UDPAddress   string `json:"udpAddress"`
}

type BusinessCheck struct {
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	LatencyMs int64  `json:"latencyMs,omitempty"`
}

type RelayBusinessResult struct {
	CheckedAt        time.Time       `json:"checkedAt"`
	State            string          `json:"state"`
	BusinessVerified bool            `json:"businessVerified"`
	Checks           []BusinessCheck `json:"checks"`
	CleanupVerified  bool            `json:"cleanupVerified"`
}
