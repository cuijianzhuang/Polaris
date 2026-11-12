package panel

import (
	"fmt"

	"github.com/cuijianzhuang/polaris/internal/panel/cdnkit"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

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
			prior := cdnkit.CachedAddressCheck(&p, key)
			if prior != nil && prior.EndpointID == ep.ID && prior.Target.Server == req.Address {
				delete(p.AddressChecks.V, key)
			}
		}
		delete(p.AddressFailures.V, cdnkit.FailureSlot(ep.ID, req.Address))
		return tx.Model(&p).Updates(map[string]any{"address_checks": p.AddressChecks, "address_failures": p.AddressFailures}).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "cdn.address_check_reset", p.Name, req.Address)
	c.JSON(200, gin.H{"ok": true})
}
