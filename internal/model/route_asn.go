package model

import "time"

type RouteTraceIPASN struct {
	State        string `json:"state"`
	IP           string `json:"ip"`
	ASN          uint32 `json:"asn,omitempty"`
	Organization string `json:"organization,omitempty"`
	Network      string `json:"network,omitempty"`
}

type RouteTraceASNMetadata struct {
	State          string    `json:"state"` // disabled / unavailable / ready
	Reason         string    `json:"reason,omitempty"`
	DatabaseSHA256 string    `json:"databaseSha256,omitempty"`
	DatabaseType   string    `json:"databaseType,omitempty"`
	BuildEpoch     uint      `json:"buildEpoch,omitempty"`
	IPVersion      uint      `json:"ipVersion,omitempty"`
	At             time.Time `json:"at"`
	Limited        bool      `json:"limited,omitempty"`
}
