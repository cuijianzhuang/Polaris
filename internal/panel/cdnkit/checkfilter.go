package cdnkit

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Target is where a client of a CDN endpoint connects, as the subscription
// tells it (see panel's expandEndpoint).
type Target struct {
	Provider   string `json:"provider,omitempty"`
	Scheme     string `json:"scheme"`
	OriginPort int    `json:"originPort"`
	Server     string `json:"server"`
	Port       int    `json:"port"`
	SNI        string `json:"sni"`
	Host       string `json:"host"`
	Path       string `json:"path"`
}

// Stage is one step of an endpoint check (DNS, TCP, TLS, HTTP, transport).
type Stage struct {
	Kind      string   `json:"kind"`
	State     string   `json:"state"`
	Detail    string   `json:"detail,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

// Check is the outcome of probing one endpoint target.
type Check struct {
	Stages   []Stage  `json:"stages,omitempty"`
	Partial  bool     `json:"partial,omitempty"`
	OK       bool     `json:"ok"`
	Status   int      `json:"status,omitempty"`
	Message  string   `json:"message"`
	Steps    []string `json:"steps"`
	Warnings []string `json:"warnings,omitempty"`
}

// AddressResult is one recorded address check: the probe outcome plus
// where and when it ran.
type AddressResult struct {
	Check
	Target           Target    `json:"target"`
	EndpointID       string    `json:"endpointId"`
	CheckedAt        time.Time `json:"checkedAt"`
	Viewpoint        string    `json:"viewpoint"`
	BusinessVerified bool      `json:"businessVerified"`
}

// FailureSlot is the stable slot of a per entry/address failure; the
// fingerprint inside the record scopes the verdict to the exact protocol,
// port, SNI, Host, path and inbound security settings.
func FailureSlot(endpoint, address string) string {
	b, _ := json.Marshal([]string{endpoint, address})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// CheckKey fingerprints one address check: endpoint, target, transport and
// the health revision of the inbound and profile it ran against.
func CheckKey(ep string, t Target, tr model.Transport, revision string) string {
	b, _ := json.Marshal([]any{ep, t, tr, revision})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// HealthRevision fingerprints the inbound and profile settings an address
// check depends on, so a config change invalidates cached verdicts.
func HealthRevision(row store.Inbound, p *store.CDNProfile) string {
	parts := []any{row.Transport, row.Security, p.TLSMode, p.CaddyFronting}
	if Tunnel(p) {
		parts = append(parts, "tunnel", p.TunnelID)
		if p.TunnelCredentialMode == "remote" {
			parts = append(parts, "remote")
		}
	}
	b, _ := json.Marshal(parts)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// CachedAddressCheck decodes one stored address check, or nil when the
// record is missing or corrupt.
func CachedAddressCheck(p *store.CDNProfile, key string) *AddressResult {
	raw := p.AddressChecks.V[key]
	if len(raw) == 0 {
		return nil
	}
	var r AddressResult
	if json.Unmarshal(raw, &r) != nil || r.CheckedAt.IsZero() {
		return nil
	}
	return &r
}

// CheckFailed reports whether a recorded check is a failure. A partial
// result is not a failure: the address may still work.
func CheckFailed(r *AddressResult) bool { return r != nil && !r.OK && !r.Partial }

// AddressFailureAt is when the address last failed for this endpoint and
// target, or nil when it has no recorded failure.
func AddressFailureAt(p *store.CDNProfile, ep string, target Target, tr model.Transport, revision string) *time.Time {
	key := CheckKey(ep, target, tr, revision)
	if f, ok := p.AddressFailures.V[FailureSlot(ep, target.Server)]; ok && f.Fingerprint == key {
		at := f.At
		return &at
	}
	// Already saved failures take effect after upgrading, without another probe.
	if r := CachedAddressCheck(p, key); CheckFailed(r) {
		at := r.CheckedAt
		return &at
	}
	return nil
}

// RecordAddressVerdict folds one check result into the profile's compact
// failure set. Compact failures are independent of the 64-entry
// detail/history limit. A new partial result cannot clear a known failure;
// only a successful recheck can.
func RecordAddressVerdict(p *store.CDNProfile, key string, result AddressResult) {
	failures := p.AddressFailures.V
	if failures == nil {
		failures = map[string]store.CDNAddressFailure{}
	}
	// Promote historical failures before their detailed records can be evicted.
	for fingerprint := range p.AddressChecks.V {
		r := CachedAddressCheck(p, fingerprint)
		if !CheckFailed(r) {
			continue
		}
		slot := FailureSlot(r.EndpointID, r.Target.Server)
		if old, ok := failures[slot]; !ok || old.At.Before(r.CheckedAt) {
			failures[slot] = store.CDNAddressFailure{Fingerprint: fingerprint, At: r.CheckedAt}
		}
	}
	slot := FailureSlot(result.EndpointID, result.Target.Server)
	switch {
	case result.OK:
		delete(failures, slot)
	case !result.Partial:
		failures[slot] = store.CDNAddressFailure{Fingerprint: key, At: result.CheckedAt}
	}
	p.AddressFailures = store.NewJSON(failures)
}
