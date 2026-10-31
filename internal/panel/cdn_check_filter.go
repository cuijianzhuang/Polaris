package panel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Stable per entry/address; the fingerprint inside the record scopes the verdict
// to the exact protocol, port, SNI, Host, path and inbound security settings.
func cdnFailureSlot(endpoint, address string) string {
	b, _ := json.Marshal([]string{endpoint, address})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func cdnCachedAddressCheck(p *store.CDNProfile, key string) *cdnAddressResult {
	raw := p.AddressChecks.V[key]
	if len(raw) == 0 {
		return nil
	}
	var r cdnAddressResult
	if json.Unmarshal(raw, &r) != nil || r.CheckedAt.IsZero() {
		return nil
	}
	return &r
}
func cdnCheckFailed(r *cdnAddressResult) bool { return r != nil && !r.OK && !r.Partial }
func cdnAddressFailureAt(p *store.CDNProfile, ep string, target cdnTarget, tr model.Transport, revision string) *time.Time {
	key := cdnCheckKey(ep, target, tr, revision)
	if f, ok := p.AddressFailures.V[cdnFailureSlot(ep, target.Server)]; ok && f.Fingerprint == key {
		at := f.At
		return &at
	}
	// Already saved failures take effect after upgrading, without another probe.
	if r := cdnCachedAddressCheck(p, key); cdnCheckFailed(r) {
		at := r.CheckedAt
		return &at
	}
	return nil
}

// Compact failures are independent of the 64-entry detail/history limit. A new
// partial result cannot clear a known failure; only a successful recheck can.
func recordCDNAddressVerdict(p *store.CDNProfile, key string, result cdnAddressResult) {
	failures := p.AddressFailures.V
	if failures == nil {
		failures = map[string]store.CDNAddressFailure{}
	}
	// Promote historical failures before their detailed records can be evicted.
	for fingerprint := range p.AddressChecks.V {
		r := cdnCachedAddressCheck(p, fingerprint)
		if !cdnCheckFailed(r) {
			continue
		}
		slot := cdnFailureSlot(r.EndpointID, r.Target.Server)
		if old, ok := failures[slot]; !ok || old.At.Before(r.CheckedAt) {
			failures[slot] = store.CDNAddressFailure{Fingerprint: fingerprint, At: r.CheckedAt}
		}
	}
	slot := cdnFailureSlot(result.EndpointID, result.Target.Server)
	switch {
	case result.OK:
		delete(failures, slot)
	case !result.Partial:
		failures[slot] = store.CDNAddressFailure{Fingerprint: key, At: result.CheckedAt}
	}
	p.AddressFailures = store.NewJSON(failures)
}
