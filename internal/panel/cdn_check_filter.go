package panel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
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

// Reset is explicit operator intent to return this target to an untested state,
// useful for protocols whose available diagnostic can only yield partial evidence.
func (s *Server) resetCDNAddressCheck(c *gin.Context) {
	var req struct {
		EndpointID string `json:"endpointId"`
		Address    string `json:"address"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.EndpointID == "" || req.Address == "" || len(req.Address) > 253 {
		badRequest(c, fmt.Errorf("请指定入口和地址"))
		return
	}
	var p store.CDNProfile
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		if err := tx.First(&p, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		var ep store.Endpoint
		if err := tx.First(&ep, "id = ? AND cdn_profile_id = ? AND kind = ?", req.EndpointID, p.ID, "cdn").Error; err != nil {
			return err
		}

		// Clear this entry/address across its historical setting snapshots. No
		// query may escape tx: SQLite intentionally has one open connection.
		for key := range p.AddressChecks.V {
			prior := cdnCachedAddressCheck(&p, key)
			if prior != nil && prior.EndpointID == ep.ID && prior.Target.Server == req.Address {
				delete(p.AddressChecks.V, key)
			}
		}
		delete(p.AddressFailures.V, cdnFailureSlot(ep.ID, req.Address))
		return tx.Model(&p).Updates(map[string]any{"address_checks": p.AddressChecks, "address_failures": p.AddressFailures}).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "cdn.address_check_reset", p.Name, req.Address)
	c.JSON(200, gin.H{"ok": true})
}
