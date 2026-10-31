package panel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type cdnCheckContext struct {
	Revision        string          `json:"-"`
	ExplicitAddress string          `json:"explicitAddress,omitempty"`
	EndpointID      string          `json:"endpointId"`
	InboundID       string          `json:"inboundId"`
	Label           string          `json:"label"`
	Transport       model.Transport `json:"transport"`
	Target          cdnTarget       `json:"target"`
	Warnings        []string        `json:"warnings"`
}
type cdnAddressResult struct {
	cdnCheck
	Target           cdnTarget `json:"target"`
	EndpointID       string    `json:"endpointId"`
	CheckedAt        time.Time `json:"checkedAt"`
	Viewpoint        string    `json:"viewpoint"`
	BusinessVerified bool      `json:"businessVerified"`
}
type cdnAddressRow struct {
	BlockKey    string             `json:"blockKey,omitempty"`
	Address     string             `json:"address"`
	Type        string             `json:"type"`
	Source      string             `json:"source"`
	Fixed       bool               `json:"fixed"`
	Blocked     bool               `json:"blocked"`
	Selected    bool               `json:"selected"`
	Reason      string             `json:"reason"`
	Sources     []string           `json:"sources"`
	ReportedAt  *time.Time         `json:"reportedAt,omitempty"`
	Measurement *store.CDNIPResult `json:"measurement,omitempty"`
	Stale       bool               `json:"stale"`
	Check       *cdnAddressResult  `json:"check,omitempty"`
	FailedAt    *time.Time         `json:"failedAt,omitempty"`
}

func cdnHealthRevision(row store.Inbound, p *store.CDNProfile) string {
	b, _ := json.Marshal([]any{row.Transport, row.Security, p.TLSMode, p.CaddyFronting})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func cdnCheckKey(ep string, t cdnTarget, tr model.Transport, revision string) string {
	b, _ := json.Marshal([]any{ep, t, tr, revision})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func (s *Server) cdnContexts(p *store.CDNProfile) []cdnCheckContext {
	var es []store.Endpoint
	s.st.DB.Where("cdn_profile_id = ? AND kind = ?", p.ID, "cdn").Order("id").Find(&es)
	out := []cdnCheckContext{}
	for _, ep := range es {
		var row store.Inbound
		if s.st.DB.First(&row, "id = ? AND enabled = ?", ep.InboundID, true).Error != nil {
			continue
		}
		m := s.toModelInbound(&row)
		if !m.CDNCapable() {
			continue
		}
		var n store.Node
		s.st.DB.First(&n, "id = ?", row.NodeID)
		out = append(out, cdnCheckContext{Revision: cdnHealthRevision(row, p), ExplicitAddress: ep.Address, EndpointID: ep.ID, InboundID: row.ID, Label: n.Name + " / " + row.Tag, Transport: m.Transport.Type, Target: s.cdnTargetFor(&ep, &m, p), Warnings: s.cdnEndpointWarnings(&ep, &row, p)})
	}
	return out
}

// The same expansion function as a real subscription, with no user credentials.
func (s *Server) cdnPreview(p *store.CDNProfile, ctx cdnCheckContext, carrier string, region string) []string {
	var ep store.Endpoint
	var row store.Inbound
	if s.st.DB.First(&ep, "id = ?", ctx.EndpointID).Error != nil || s.st.DB.First(&row, "id = ?", ctx.InboundID).Error != nil {
		return nil
	}
	m := s.toModelInbound(&row)
	nodes := s.expandEndpoint(sub.Node{Port: m.Port, Transport: m.Transport}, ep, map[string]string{}, "{kind}", nil, nil, &m, carrier, cdnAudience{Region: region})
	out := []string{}
	for _, n := range nodes {
		out = append(out, n.Server)
	}
	return out
}
func (s *Server) cdnAddressTools(c *gin.Context) {
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	carrier := c.Query("carrier")
	if carrier != "" {
		if _, ok := cdnCarriers[carrier]; !ok {
			badRequest(c, fmt.Errorf("carrier 只能是 ct、cu、cm 或留空"))
			return
		}
	}
	region, err := normalizeCDNRegion(c.Query("region"))
	if err != nil {
		badRequest(c, err)
		return
	}
	contexts := s.cdnContexts(&p)
	var chosen *cdnCheckContext
	for i := range contexts {
		if (c.Query("endpoint") == "" && i == 0) || contexts[i].EndpointID == c.Query("endpoint") {
			chosen = &contexts[i]
			break
		}
	}
	if c.Query("endpoint") != "" && chosen == nil {
		badRequest(c, fmt.Errorf("请选择当前配置档的有效 CDN 入口"))
		return
	}
	port := cdnDefaultPort(&p)
	selected := []string{}
	if chosen != nil {
		port = chosen.Target.Port
		selected = s.cdnPreview(&p, *chosen, carrier, region)
	}
	addresses := append(slices.Clone(s.cdnAddressesForPort(&p, port)), p.PinnedHosts.V...)
	addresses = append(addresses, p.BlockedHosts.V...)
	addresses = append(addresses, selected...)
	reportCarrier := carrier
	if !p.CarrierSplit {
		reportCarrier = ""
	}
	reports := cdnReportsForPort(s.st.DB, &p, port, time.Time{}, reportCarrier)
	seen := map[string]bool{}
	items := []cdnAddressRow{}
	for _, a := range addresses {
		if seen[a] {
			continue
		}
		seen[a] = true
		row := cdnAddressRow{Address: a, Type: "domain", Source: "retained", Fixed: slices.Contains(p.PinnedHosts.V, a), Blocked: cdnAddressBlocked(&p, a), Selected: slices.Contains(selected, a), Sources: []string{}}
		if ip := net.ParseIP(a); ip != nil {
			row.Type = "ipv6"
			if ip.To4() != nil {
				row.Type = "ipv4"
			}
		}
		for _, report := range reports {
			for _, m := range report.IPs.V {
				if m.IP == a {
					row.Sources = append(row.Sources, report.Source)
					if row.ReportedAt == nil || row.ReportedAt.Before(report.At) {
						at := report.At
						row.ReportedAt = &at
						v := m
						row.Measurement = &v
						row.Stale = time.Since(at) > cdnStaleAfter(&p)
					}
				}
			}
		}
		for _, b := range p.BlockedHosts.V {
			if cdnAddressBlocked(&store.CDNProfile{BlockedHosts: store.NewJSON([]string{b})}, a) {
				row.BlockKey = b
				break
			}
		}
		if row.Fixed {
			row.Source = "fixed"
		} else if row.ReportedAt != nil {
			row.Source = "measurement"
		} else if p.IPSourceURL != "" {
			row.Source = "source_or_retained"
		}
		if chosen != nil {
			target := chosen.Target
			target.Server = a
			row.FailedAt = cdnAddressFailureAt(&p, chosen.EndpointID, target, chosen.Transport, chosen.Revision)
		}
		switch {
		case row.Blocked:
			row.Reason = "blocked"
		case chosen == nil:
			row.Reason = "no_endpoint"
		case row.FailedAt != nil:
			row.Reason = "check_failed"
		case row.Selected && chosen.ExplicitAddress != "":
			row.Reason = "entry_override"
		case row.Selected && chosen.Target.Server == a && (!p.ExpandPerIP):
			row.Reason = "origin_only"
		case row.Selected && row.Fixed:
			row.Reason = "fixed"
		case row.Selected:
			row.Reason = "selected"
		default:
			row.Reason = "not_selected"
		}
		if chosen != nil {
			target := chosen.Target
			target.Server = a
			row.Check = cdnCachedAddressCheck(&p, cdnCheckKey(chosen.EndpointID, target, chosen.Transport, chosen.Revision))
		}
		items = append(items, row)
	}
	_, matchMode := s.cdnAudienceReports(&p, port, carrier, region)
	c.JSON(200, gin.H{"contexts": contexts, "region": region, "regions": s.cdnRegionOptions(), "matchMode": matchMode, "selectionMessage": s.cdnSelectionText(&p, carrier, region, nil, port), "context": chosen, "items": items, "selected": selected, "scheme": cdnScheme(&p), "port": port, "carrier": carrier, "stalePolicy": orDefault(p.StalePolicy, "source"), "staleHours": int(cdnStaleAfter(&p).Hours()), "expandPerIp": p.ExpandPerIP, "ipTopN": p.IPTopN})
}
func (s *Server) checkCDNAddress(c *gin.Context) {
	var req struct {
		EndpointID string `json:"endpointId"`
		Address    string `json:"address"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var ep store.Endpoint
	if err := s.st.DB.First(&ep, "id = ? AND cdn_profile_id = ? AND kind = ?", req.EndpointID, p.ID, "cdn").Error; err != nil {
		dbError(c, err)
		return
	}
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ? AND enabled = ?", ep.InboundID, true).Error; err != nil {
		dbError(c, err)
		return
	}
	m := s.toModelInbound(&row)
	target := s.cdnTargetFor(&ep, &m, &p)
	if !m.CDNCapable() {
		badRequest(c, fmt.Errorf("此入口不支持 CDN 检测"))
		return
	}
	candidates := append(slices.Clone(s.cdnAddressesForPort(&p, target.Port)), p.PinnedHosts.V...)
	candidates = append(candidates, p.BlockedHosts.V...)
	candidates = append(candidates, target.Server)
	if req.Address == "" || !slices.Contains(candidates, req.Address) {
		badRequest(c, fmt.Errorf("只能检测当前配置档保存的候选地址"))
		return
	}
	target.Server = req.Address
	result := cdnAddressResult{Target: target, EndpointID: ep.ID, CheckedAt: time.Now(), Viewpoint: "panel"}
	if problem := s.cdnEndpointProblem(&m, row.ID, &p, ep.Port); problem != "" {
		result.Message = problem
		result.Steps = []string{}
	} else {
		var n store.Node
		s.st.DB.First(&n, "id = ?", row.NodeID)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
		defer cancel()
		result.cdnCheck = probeCDN(ctx, target, m.Transport.Type, nodeOrigin(&n))
	}
	result.Warnings = s.cdnEndpointWarnings(&ep, &row, &p)
	key := cdnCheckKey(ep.ID, target, m.Transport.Type, cdnHealthRevision(row, &p))
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		var current store.CDNProfile
		if err := tx.First(&current, "id = ?", p.ID).Error; err != nil {
			return err
		}
		if failure, ok := current.AddressFailures.V[cdnFailureSlot(result.EndpointID, result.Target.Server)]; ok && failure.At.After(result.CheckedAt) {
			return nil
		}
		// A slower, older request cannot undo a newer check of the same address.
		for fingerprint := range current.AddressChecks.V {
			prior := cdnCachedAddressCheck(&current, fingerprint)
			if prior != nil && prior.EndpointID == result.EndpointID && prior.Target.Server == result.Target.Server && prior.CheckedAt.After(result.CheckedAt) {
				return nil
			}
		}
		recordCDNAddressVerdict(&current, key, result)
		checks := current.AddressChecks.V
		if checks == nil {
			checks = map[string]json.RawMessage{}
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		checks[key] = raw
		// Bounded history; results from changed entry settings have different keys.
		type old struct {
			key string
			at  time.Time
		}
		order := []old{}
		for k, v := range checks {
			var r cdnAddressResult
			_ = json.Unmarshal(v, &r)
			order = append(order, old{k, r.CheckedAt})
		}
		sort.Slice(order, func(i, j int) bool { return order[i].at.Before(order[j].at) })
		for len(order) > 64 {
			delete(checks, order[0].key)
			order = order[1:]
		}
		return tx.Model(&current).Updates(map[string]any{"address_checks": store.NewJSON(checks), "address_failures": current.AddressFailures}).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "cdn.address_check", p.Name, req.Address)
	c.JSON(http.StatusOK, result)
}
func (s *Server) updateCDNAddressPolicy(c *gin.Context) {
	var req struct {
		BlockedHosts *[]string `json:"blockedHosts"`
		StalePolicy  string    `json:"stalePolicy"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.BlockedHosts == nil || (req.StalePolicy != "keep" && req.StalePolicy != "source") {
		badRequest(c, fmt.Errorf("请提供 blockedHosts 数组和 keep/source 失效策略"))
		return
	}
	blocked, err := normalizeCDNBlockedHosts(*req.BlockedHosts)
	if err != nil {
		badRequest(c, err)
		return
	}
	var p store.CDNProfile
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		if err := tx.First(&p, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		return tx.Model(&p).Updates(map[string]any{"blocked_hosts": store.NewJSON(blocked), "stale_policy": req.StalePolicy}).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "cdn.address_policy", p.Name, req.StalePolicy)
	c.JSON(200, gin.H{"ok": true})
}
