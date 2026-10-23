package panel

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Publication is separate from the node's listener / firewall configuration.
// Keep the original endpoints so turning the policy off restores that set.
func (s *Server) setInboundPublication(c *gin.Context) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var req struct {
		RelayOnly *bool `json:"relayOnly" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var in store.Inbound
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&in, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		if in.ServiceOnly {
			return badInput{fmt.Errorf("内部服务入站不对用户发布")}
		}
		if in.RelayAccess.V.Enabled && !*req.RelayOnly {
			return badInput{fmt.Errorf("请先关闭中转来源限制，再允许发布直连入口")}
		}
		res := tx.Model(&store.Inbound{}).Where("id = ?", in.ID).Update("relay_only", *req.RelayOnly)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return tx.Create(&store.AuditLog{Admin: c.GetString("admin"), Action: "inbound.publication", Target: in.Tag, Detail: "relayOnly=" + strconv.FormatBool(*req.RelayOnly), IP: clientIP(c.Request)}).Error
	})
	if err != nil {
		relaySetupError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"inboundId": in.ID, "relayOnly": *req.RelayOnly})
}
