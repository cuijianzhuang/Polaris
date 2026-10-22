package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/warp"
)

// addWARP is POST /nodes/:id/warp {tag, license, rules, priority}: registers
// a free Cloudflare WARP device from the panel, saves it as a WireGuard
// outbound on the node and, when rules name any traffic, routes that
// traffic through it.
func (s *Server) addWARP(c *gin.Context) {
	nodeID := c.Param("id")
	var req struct {
		Tag      string           `json:"tag"`
		License  string           `json:"license"`
		Rules    model.RouteMatch `json:"rules"`
		Priority int              `json:"priority"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	req.Tag = strings.TrimSpace(req.Tag)
	if req.Tag == "" {
		req.Tag = "warp"
	}
	if req.Priority == 0 {
		req.Priority = 25
	}
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", nodeID).Error; err != nil {
		dbError(c, err)
		return
	}
	var n int64
	s.st.DB.Model(&store.Outbound{}).Where("node_id = ? AND tag = ?", nodeID, req.Tag).Count(&n)
	if n > 0 || req.Tag == "direct" || req.Tag == "block" {
		badRequest(c, fmt.Errorf("标签 %q 已被占用，换一个", req.Tag))
		return
	}
	// Check before registering: a device per failed attempt is wasted.
	if err := s.nodeSupportsOutbound(nodeID, model.OutWireGuard); err != nil {
		badRequest(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	dev, err := warp.Register(ctx, req.License)
	if err != nil {
		badRequest(c, err)
		return
	}
	set, _ := json.Marshal(model.OutboundSettings{Server: dev.Endpoint, Port: warp.Port, PrivateKey: dev.PrivateKey,
		PeerPublicKey: dev.PeerPublicKey, LocalAddress: dev.LocalAddresses(), Reserved: dev.Reserved, MTU: warp.MTU})
	remark := "Cloudflare WARP"
	if dev.WarpPlus {
		remark = "Cloudflare WARP+"
	}
	o := store.Outbound{ID: store.NewID()}
	if err := s.storeOutbound(nodeID, &o, &outboundReq{Tag: req.Tag, Type: model.OutWireGuard, Settings: set, Remark: remark}); err != nil {
		var bad badInput
		if errors.As(err, &bad) {
			badRequest(c, bad.error)
		} else {
			dbError(c, err)
		}
		return
	}
	ruled := !emptyMatch(req.Rules)
	if ruled {
		if err := s.st.DB.Create(&store.RoutingRule{ID: store.NewID(), NodeID: nodeID, Match: store.NewJSON(req.Rules),
			OutboundTag: req.Tag, Priority: req.Priority, Remark: "WARP 分流"}).Error; err != nil {
			serverError(c, err)
			return
		}
		s.markRoutingDirty(nodeID)
	}
	s.auditCtx(c, "outbound.warp", node.Name+" / "+req.Tag, fmt.Sprintf("%s, rule %v", dev.AccountType, ruled))
	c.JSON(http.StatusOK, gin.H{"outbound": o, "device": dev, "ruleAdded": ruled})
}

func emptyMatch(m model.RouteMatch) bool {
	return len(m.Geosite) == 0 && len(m.GeoIP) == 0 && len(m.Domain) == 0 && len(m.DomainSuffix) == 0 &&
		len(m.DomainKeyword) == 0 && len(m.IPCIDR) == 0 && len(m.RuleSets) == 0
}
