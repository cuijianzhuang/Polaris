package panel

import (
	"fmt"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A referenced tag cannot be renamed or removed: leaving it dangling can
// silently send traffic through a core's default outbound instead.
func (s *Server) checkOutboundReferences(db *gorm.DB, nodeID, selfID, tag string) error {
	var obs []store.Outbound
	if err := db.Where("node_id = ? AND id <> ?", nodeID, selfID).Find(&obs).Error; err != nil {
		return err
	}
	for _, o := range obs {
		m := model.Outbound{Tag: o.Tag, Type: o.Type, Settings: o.Settings, Detour: o.Detour}
		deps, err := m.OutboundDependencies()
		if err != nil {
			return err
		}
		for _, dep := range deps {
			if dep == tag {
				return fmt.Errorf("出站 %q 仍被 %q 的成员、兜底或下一跳引用，请先解除引用", tag, o.Tag)
			}
		}
	}
	var n int64
	if err := db.Model(&store.Inbound{}).Where("node_id = ? AND outbound_tag = ?", nodeID, tag).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("出站 %q 仍被入站绑定，请先解除绑定", tag)
	}
	if err := db.Model(&store.RoutingRule{}).Where("node_id IN ? AND outbound_tag = ?", []string{nodeID, model.GlobalNodeID}, tag).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("出站 %q 仍被路由规则引用，请先修改规则", tag)
	}
	return nil
}

func (s *Server) validateOutboundChange(db *gorm.DB, nodeID, selfID string, req *outboundReq) error {
	var rows []store.Outbound
	if err := db.Where("node_id = ?", nodeID).Find(&rows).Error; err != nil {
		return err
	}
	obs := make([]model.Outbound, 0, len(rows)+1)
	for _, o := range rows {
		if o.ID == selfID {
			if o.Tag != req.Tag {
				if err := s.checkOutboundReferences(db, nodeID, selfID, o.Tag); err != nil {
					return err
				}
			}
			continue
		}
		obs = append(obs, model.Outbound{Tag: o.Tag, Type: o.Type, Settings: o.Settings, Detour: o.Detour})
	}
	obs = append(obs, model.Outbound{Tag: req.Tag, Type: req.Type, Settings: req.Settings, Detour: req.Detour})
	return model.ValidateOutboundGraph(obs)
}

// The transaction lock also serializes graph changes across panel replicas.
// SQLite protects competing writers; PostgreSQL needs an explicit per-node
// advisory lock before reading the graph under READ COMMITTED.
func (s *Server) mutateOutbounds(nodeID string, fn func(*gorm.DB) error) error {
	s.outboundMu.Lock()
	defer s.outboundMu.Unlock()
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := store.ReserveSourceResources(tx); e != nil {
			return e
		}
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "polaris-outbounds:"+nodeID).Error; err != nil {
				return err
			}
		}
		return fn(tx)
	})
}
