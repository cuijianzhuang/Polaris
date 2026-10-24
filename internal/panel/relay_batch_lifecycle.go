package panel

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A creation batch owns independent paths. Never infer membership from node
// names or shared tail nodes, and never mutate paths belonging to another batch.
func loadInboundRelayBatch(tx *gorm.DB, inboundID, batchID string) ([]store.Chain, error) {
	var chains []store.Chain
	if batchID == "" {
		return nil, gorm.ErrRecordNotFound
	}
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("batch_id = ? AND managed = ?", batchID, true).Order("id").Find(&chains).Error; err != nil {
		return nil, err
	}
	if len(chains) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	for _, ch := range chains {
		hops := ch.Hops.V
		if len(hops) < 2 || hops[len(hops)-1].InboundID != inboundID {
			return nil, gorm.ErrRecordNotFound
		}
	}
	return chains, nil
}

func (s *Server) relayBatchViews(batchID string, in store.Inbound, chains []store.Chain) gin.H {
	views := make([]inboundRelayView, 0, len(chains))
	state := ""
	for _, ch := range chains {
		view := s.relayView(ch, in)
		if state == "" {
			state = view.State
		} else if state != view.State {
			state = "mixed"
		}
		views = append(views, view)
	}
	return gin.H{"batchId": batchID, "state": state, "chains": views}
}

func (s *Server) getInboundRelayBatch(c *gin.Context) {
	var in store.Inbound
	var chains []store.Chain
	err := s.st.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&in, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		var err error
		chains, err = loadInboundRelayBatch(tx, in.ID, c.Param("bid"))
		return err
	})
	if err != nil {
		relaySetupError(c, err)
		return
	}
	c.JSON(http.StatusOK, s.relayBatchViews(c.Param("bid"), in, chains))
}

func (s *Server) setInboundRelayBatchState(c *gin.Context) {
	var req struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var in store.Inbound
	var chains []store.Chain
	var affected []string
	err := s.st.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&in, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		var err error
		chains, err = loadInboundRelayBatch(tx, in.ID, c.Param("bid"))
		if err != nil {
			return err
		}
		action := "disable"
		if *req.Enabled {
			action = "enable"
		}
		if err = checkRelayChange(tx, c, "batch", action, chains); err != nil {
			return err
		}
		if *req.Enabled {
			if _, err := relayLanding(tx, in.ID); err != nil {
				return badInput{err}
			}
			if err := validateRelayAccessInbound(tx, in); err != nil {
				return badInput{err}
			}
		}
		for i := range chains {
			ch := &chains[i]
			if ch.Disabled == !*req.Enabled {
				continue
			}
			nodes, err := relayNodes(tx, ch)
			if err != nil {
				return err
			}
			if *req.Enabled {
				if _, err := checkRelayAccessChain(tx, in, ch); err != nil {
					return badInput{err}
				}
			}
			ch.Disabled = !*req.Enabled
			ch.TargetRevs = store.NewJSON(relayTargetRevs(ch, nodes))
			if err := tx.Model(ch).Where("id = ?", ch.ID).Updates(map[string]any{"disabled": ch.Disabled, "target_revs": ch.TargetRevs}).Error; err != nil {
				return err
			}
			affected = append(affected, chainNodes(ch)...)
		}
		if len(affected) == 0 {
			return nil
		}
		audit := store.AuditLog{Admin: c.GetString("admin"), Action: "relay.batch.state", Target: c.Param("bid"), Detail: strconv.FormatBool(*req.Enabled), IP: clientIP(c.Request)}
		return tx.Create(&audit).Error
	})
	if err != nil {
		relayChangeError(c, err)
		return
	}
	if len(affected) > 0 {
		s.MarkDirty("relay batch state changed", affected...)
	}
	c.JSON(http.StatusOK, s.relayBatchViews(c.Param("bid"), in, chains))
}

func (s *Server) deleteInboundRelayBatch(c *gin.Context) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var chains []store.Chain
	err := s.st.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var err error
		chains, err = loadInboundRelayBatch(tx, c.Param("id"), c.Param("bid"))
		if err != nil {
			return err
		}
		if err = checkRelayChange(tx, c, "batch", "delete", chains); err != nil {
			return err
		}
		for _, ch := range chains {
			if err := s.dematerializeChain(tx, &ch); err != nil {
				return err
			}
			if err := tx.Delete(&store.Chain{}, "id = ?", ch.ID).Error; err != nil {
				return err
			}
		}
		audit := store.AuditLog{Admin: c.GetString("admin"), Action: "relay.batch.delete", Target: c.Param("bid"), Detail: fmt.Sprintf("%d paths", len(chains)), IP: clientIP(c.Request)}
		return tx.Create(&audit).Error
	})
	if err != nil {
		relayChangeError(c, err)
		return
	}
	var affected []string
	for _, ch := range chains {
		affected = append(affected, chainNodes(&ch)...)
	}
	s.MarkDirty("relay batch deleted", affected...)
	c.Status(http.StatusNoContent)
}
