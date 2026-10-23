package panel

import (
	"fmt"
	"net/http"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

func (s *Server) checkInboundOutboundIP(n *store.Node, in *model.Inbound) error {
	if err := in.OutboundIPStrategy.Validate(); err != nil {
		return err
	}
	if in.OutboundIPStrategy.IsAuto() {
		return nil
	}
	if !contains(n.AgentFeatures.V, model.OutboundIPFeature) {
		return fmt.Errorf("节点 %s 需要先升级 Agent 并完成一次配置拉取，才能设置出站 IP 策略", n.Name)
	}
	if err := core.CheckOutboundIPDNS(in.Core, s.dnsSpec(n.ID)); err != nil {
		return err
	}
	// Check the current graph before saving a policy; the Agent checks again
	// when applying later routing changes.
	obs, err := s.buildOutbounds(n.ID)
	if err != nil {
		return err
	}
	routes, err := s.buildRoutes(n.ID)
	if err != nil {
		return err
	}
	version := ""
	cores, err := s.nodeCores(n.ID)
	if err != nil {
		return err
	}
	for _, c := range cores {
		if c.Type == in.Core {
			version = c.Version
		}
	}
	probe := *in
	probe.Enabled = true
	if probe.ID != "" {
		// Per-inbound egress bindings resolve against the stored tag. Account
		// for an unsaved rename during an ordinary inbound edit.
		var old store.Inbound
		if s.st.DB.First(&old, "id = ?", probe.ID).Error == nil {
			for i := range routes {
				for j, tag := range routes[i].Match.Inbound {
					if tag == old.Tag {
						routes[i].Match.Inbound[j] = probe.Tag
					}
				}
			}
		}
	}
	_, err = core.PlanEgress(&model.NodeSpec{Inbounds: []model.Inbound{probe}, Outbounds: obs, Routes: routes, DNS: s.dnsSpec(n.ID)}, in.Core, version, outboundIPPreviewWire())
	return err
}

// Separate updates preserve the policy when older editors omit the field.
func (s *Server) setOutboundIP(c *gin.Context) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var req struct {
		Strategy *model.OutboundIPStrategy `json:"strategy"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Strategy == nil {
		badRequest(c, fmt.Errorf("strategy is required"))
		return
	}
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", row.NodeID).Error; err != nil {
		dbError(c, err)
		return
	}
	in := s.toModelInbound(&row)
	in.OutboundIPStrategy = *req.Strategy
	if err := s.validateInbound(&n, &in, row.ID); err != nil {
		badRequest(c, err)
		return
	}
	fields := row.OverriddenFields.V
	if row.TemplateID != "" {
		fields = mergeFields(fields, []string{"outboundIPStrategy"})
	}
	if err := s.st.DB.Model(&row).Updates(map[string]any{"outbound_ip_strategy": in.OutboundIPStrategy, "overridden_fields": store.NewJSON(fields)}).Error; err != nil {
		serverError(c, err)
		return
	}
	s.MarkDirty("outbound IP strategy changed", row.NodeID)
	s.auditCtx(c, "inbound.outbound_ip", row.ID, string(in.OutboundIPStrategy))
	c.JSON(http.StatusOK, gin.H{"strategy": in.OutboundIPStrategy})
}

// Validate the proposed DNS mode before saving it, including inherited global
// settings. Existing per-inbound restrictions must not silently stop working.
func (s *Server) checkOutboundIPDNSChange(scope, mode string, proposed *model.DNSSpec) error {
	var ins []store.Inbound
	if err := s.st.DB.Where("outbound_ip_strategy <> ? AND outbound_ip_strategy <> ?", "", "auto").Find(&ins).Error; err != nil {
		return err
	}
	for _, in := range ins {
		var d *model.DNSSpec
		if scope == model.GlobalNodeID {
			var local store.DNSConfig
			err := s.st.DB.First(&local, "scope = ?", in.NodeID).Error
			if err == nil && local.Mode != "inherit" && local.Mode != "" {
				continue
			}
			if mode == "on" {
				d = proposed
			}
		} else {
			if in.NodeID != scope {
				continue
			}
			if mode == "custom" {
				d = proposed
			} else if mode == "inherit" {
				var global store.DNSConfig
				if s.st.DB.First(&global, "scope = ?", model.GlobalNodeID).Error == nil && global.Mode == "on" {
					d = &global.Spec.V
				}
			}
		}
		if err := core.CheckOutboundIPDNS(in.Core, d); err != nil {
			return fmt.Errorf("入站 %s: %w", in.Tag, err)
		}
	}
	return nil
}

// Native previews are not executed. These loopback placeholders show the
// routing round trip without publishing a live limiter credential.
func outboundIPPreviewWire() *model.LimitWire {
	return &model.LimitWire{Limiter: "127.0.0.1:10086", Reentry: "127.0.0.1:10087", Secret: "preview-only", Contexts: true, ReentryProtocol: "vless"}
}
