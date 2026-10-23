package panel

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Existing nodes inherit without a migration that enables new probing traffic.
func nodeProbeTargets(n *store.Node, global []store.ProbeTarget) []store.ProbeTarget {
	switch n.ProbeMode {
	case "", "inherit":
		return global
	case "custom":
		return n.ProbeTargets.V
	default:
		return nil // off, or unknown stored policy: fail closed
	}
}

func probePolicyView(n *store.Node, global []store.ProbeTarget) gin.H {
	mode := n.ProbeMode
	if mode == "" {
		mode = "inherit"
	}
	return gin.H{"mode": mode, "targets": nonNil(n.ProbeTargets.V), "effectiveTargets": nonNil(nodeProbeTargets(n, global))}
}

func (s *Server) getNodeProbePolicy(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	s.seedProbeTargets()
	c.JSON(http.StatusOK, probePolicyView(&n, s.latencyTargets(true)))
}

func (s *Server) putNodeProbePolicy(c *gin.Context) {
	var r struct {
		Mode    string           `json:"mode"`
		Targets []probeTargetReq `json:"targets"`
	}
	if err := c.ShouldBindJSON(&r); err != nil {
		badRequest(c, err)
		return
	}
	if r.Mode != "inherit" && r.Mode != "custom" && r.Mode != "off" {
		badRequest(c, errors.New("mode 只能是 inherit、custom 或 off"))
		return
	}
	if len(r.Targets) > 20 || (r.Mode == "custom" && len(r.Targets) == 0) || (r.Mode != "custom" && len(r.Targets) != 0) {
		badRequest(c, errors.New("custom 需要 1–20 个测试点；inherit/off 不接受测试点"))
		return
	}
	seen := map[string]bool{}
	for i := range r.Targets {
		t := &r.Targets[i]
		if err := t.check(); err != nil {
			badRequest(c, err)
			return
		}
		key := strings.ToLower(t.Addr)
		if seen[key] || (t.Enabled != nil && !*t.Enabled) {
			badRequest(c, errors.New("测试点地址不能重复，关闭测试请使用 off"))
			return
		}
		seen[key] = true
	}
	var updated store.Node
	err := s.withNodeNet(c.Param("id"), func(tx *gorm.DB, n *store.Node) error {
		targets := make([]store.ProbeTarget, 0, len(r.Targets))
		for i, t := range r.Targets {
			id := ""
			created := time.Now().UTC()
			for _, old := range n.ProbeTargets.V {
				if old.Addr == t.Addr {
					id, created = old.ID, old.CreatedAt
					break
				}
			}
			if id == "" {
				id = store.NewID()
			}
			targets = append(targets, store.ProbeTarget{ID: id, Name: t.Name, Carrier: t.Carrier, Addr: t.Addr, Enabled: true, SortOrder: i, CreatedAt: created})
		}
		// Keep history only for retained addresses. New targets get new IDs,
		// so results from a replaced target cannot masquerade as the new one.
		keep := []string{}
		if r.Mode == "custom" {
			for _, t := range targets {
				keep = append(keep, t.ID)
			}
		} else if r.Mode == "inherit" {
			var ts []store.ProbeTarget
			if err := tx.Where("enabled = ?", true).Find(&ts).Error; err != nil {
				return err
			}
			for _, t := range ts {
				keep = append(keep, t.ID)
			}
		}
		q := tx.Where("node_id = ?", n.ID)
		if len(keep) > 0 {
			q = q.Where("target_id NOT IN ?", keep)
		}
		if err := q.Delete(&store.NodeLatency{}).Error; err != nil {
			return err
		}
		n.ProbeMode, n.ProbeTargets = r.Mode, store.JSON[[]store.ProbeTarget]{V: targets}
		if err := tx.Model(n).Updates(map[string]any{"probe_mode": n.ProbeMode, "probe_targets": n.ProbeTargets}).Error; err != nil {
			return err
		}
		updated = *n
		return nil
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.statusCache.Store(nil)
	s.auditCtx(c, "node.probe.update", updated.Name, r.Mode)
	c.JSON(http.StatusOK, probePolicyView(&updated, s.latencyTargets(true)))
}

// Serialize against policy changes and node deletion. A late command response
// from a removed/changed target must not resurrect old measurements.
func (s *Server) recordNodeProbe(nodeID string, target store.ProbeTarget, ms float64, ok bool, now time.Time) error {
	return s.withNodeNet(nodeID, func(tx *gorm.DB, n *store.Node) error {
		var global []store.ProbeTarget
		if n.ProbeMode == "" || n.ProbeMode == "inherit" {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND enabled = ?", target.ID, true).Find(&global).Error; err != nil {
				return err
			}
		}
		for _, t := range nodeProbeTargets(n, global) {
			if t.ID == target.ID && t.Addr == target.Addr {
				return recordLatencyDB(tx, nodeID, target.ID, ms, ok, now)
			}
		}
		return nil
	})
}
