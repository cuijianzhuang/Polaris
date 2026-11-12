// Package routedb holds the Server-independent policy of a route MMDB
// database (ASN or city/geo): its on-disk snapshot is named by SHA-256,
// and the stored policy says whether it is enabled and which snapshot is
// installed. The settings row is read through the SettingStore interface
// (store.go), which package panel implements with gorm. Loading,
// uploading and serving the database stay in package panel; only the
// policy encoding lives here.
package routedb

import (
	"encoding/json"
	"regexp"
)

// Policy is the stored policy shared by both route databases: same JSON
// shape, different setting keys.
type Policy struct {
	Enabled  bool   `json:"enabled"`
	SHA256   string `json:"sha256"`
	Revision int64  `json:"revision"`
}

// Hash matches a snapshot name: a lowercase hex SHA-256.
var Hash = regexp.MustCompile(`^[a-f0-9]{64}$`)

// DecodePolicy parses a stored policy. An empty value is the default
// (disabled, no snapshot). A malformed value, a negative revision, a bad
// hash, or an enabled policy with no snapshot reports invalid, the
// caller-supplied sentinel error.
func DecodePolicy(value string, invalid error) (Policy, error) {
	var p Policy
	if value == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(value), &p); err != nil {
		return p, invalid
	}
	if p.Revision < 0 || (p.SHA256 != "" && !Hash.MatchString(p.SHA256)) || (p.Enabled && p.SHA256 == "") {
		return p, invalid
	}
	return p, nil
}
