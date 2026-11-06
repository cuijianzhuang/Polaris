package panel

import (
	"encoding/json"
	"fmt"

	"github.com/cuijianzhuang/polaris/internal/core/singbox"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func checkOutboundOnCores(cores []store.Core, o model.Outbound) error {
	for _, cr := range cores {
		if cr.Type == model.CoreSingbox {
			if err := singbox.CheckOutbound(o); err != nil {
				return fmt.Errorf("sing-box %s: %w", orDefault(cr.RunningVersion, cr.Version), err)
			}
		}
	}
	return nil
}

func nodeRefTransportError(cores []store.Core, target *model.Inbound) error {
	typ, ok := outboundTypeFor(target.Protocol.Type)
	if !ok {
		return fmt.Errorf("%s 不能作为链路下一跳", target.Protocol.Type)
	}
	settings, err := json.Marshal(model.OutboundSettings{Transport: &target.Transport, Encryption: target.Protocol.Encryption})
	if err != nil {
		return err
	}
	return checkOutboundOnCores(cores, model.Outbound{Type: typ, Settings: settings})
}

func (s *Server) checkNodeRefTransportOn(db *gorm.DB, sourceNode string, target *model.Inbound) error {
	var cores []store.Core
	if err := db.Where("node_id = ?", sourceNode).Find(&cores).Error; err != nil {
		return err
	}
	return nodeRefTransportError(cores, target)
}

func (s *Server) checkOutboundTransportOn(db *gorm.DB, sourceNode string, o model.Outbound) error {
	var cores []store.Core
	if err := db.Where("node_id = ?", sourceNode).Find(&cores).Error; err != nil {
		return err
	}
	return checkOutboundOnCores(cores, o)
}
