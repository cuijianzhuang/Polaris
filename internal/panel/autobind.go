package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Automatic domain binding: with a DNS provider and a parent zone set
// (ddns_auto_provider / ddns_auto_zone), every node gets
// <slug of its name>.<zone> when it enrols, kept pointing at its public IP
// like any other binding. ddns_auto_address also makes the domain the
// node's address, so subscriptions keep working when the IP changes.

type autoBindConfig struct {
	Provider string
	Zone     string
	Family   string
	Address  bool
}

func (s *Server) autoBindSettings() (autoBindConfig, bool) {
	c := autoBindConfig{
		Provider: s.st.GetSetting("ddns_auto_provider", ""),
		Zone:     strings.Trim(strings.ToLower(strings.TrimSpace(s.st.GetSetting("ddns_auto_zone", ""))), "."),
		Family:   s.st.GetSetting("ddns_auto_family", "ipv4"),
		Address:  s.st.GetSetting("ddns_auto_address", "true") == "true",
	}
	if c.Family != "ipv6" && c.Family != "dual" {
		c.Family = "ipv4"
	}
	return c, c.Provider != "" && c.Zone != ""
}

// domainLabel turns a node name into a DNS label: ASCII letters and digits
// kept (lowercased), everything else collapsed into single hyphens.
func domainLabel(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	l := strings.Trim(b.String(), "-")
	if len(l) > 40 {
		l = strings.Trim(l[:40], "-")
	}
	return l
}

// autoBindNode binds a node that has no domain binding yet. It returns the
// binding, or nil when automatic binding is off or the node has one.
func (s *Server) autoBindNode(ctx context.Context, nodeID string) (*store.DomainBinding, error) {
	cfg, on := s.autoBindSettings()
	if !on {
		return nil, nil
	}
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", nodeID).Error; err != nil {
		return nil, err
	}
	var have int64
	s.st.DB.Model(&store.DomainBinding{}).Where("node_id = ?", n.ID).Count(&have)
	if have > 0 {
		return nil, nil
	}
	label := domainLabel(n.Name)
	if label == "" {
		label = "node-" + strings.ToLower(n.ID[:min(6, len(n.ID))])
	}
	domain := ""
	for i := 1; i < 100; i++ {
		d := label + "." + cfg.Zone
		if i > 1 {
			d = fmt.Sprintf("%s-%d.%s", label, i, cfg.Zone)
		}
		var taken int64
		s.st.DB.Model(&store.DomainBinding{}).Where("domain = ?", d).Count(&taken)
		if taken == 0 {
			domain = d
			break
		}
	}
	if domain == "" {
		return nil, fmt.Errorf("no free name under %s for %s", cfg.Zone, n.Name)
	}
	b := store.DomainBinding{ID: store.NewID(), NodeID: n.ID, ProviderID: cfg.Provider, Domain: domain, Family: cfg.Family, TTL: 60, Status: "pending"}
	if err := s.st.DB.Create(&b).Error; err != nil {
		return nil, err
	}
	upd := map[string]any{"domain": domain}
	if cfg.Address {
		// The domain follows the IP now; the address no longer has to.
		upd["address"], upd["d_dns"] = domain, false
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(upd)
	s.MarkDirty("node domain bound automatically", n.ID)
	s.audit("system", "domain.auto_bind", domain, n.Name, "")
	err := s.syncBinding(ctx, &b, true) // pending until the agent reports an IP
	return &b, err
}

// autoBindAfterEnroll runs in the background after a node's first enrolment.
func (s *Server) autoBindAfterEnroll(nodeID string) {
	ctx, cancel := context.WithTimeout(s.ctxBG(), 60*time.Second)
	defer cancel()
	if b, err := s.autoBindNode(ctx, nodeID); err != nil {
		s.log.Warn("automatic domain binding", "node", nodeID, "err", err)
		if b == nil {
			s.notify(Event{Kind: "ddns_failed", NodeID: nodeID, Text: "自动绑定域名失败：" + err.Error()})
		}
	}
}

// autoBindAll is POST /domains/auto-bind: bind every node that has none.
func (s *Server) autoBindAll(c *gin.Context) {
	if _, on := s.autoBindSettings(); !on {
		badRequest(c, fmt.Errorf("先在「自动绑定」里选择 DNS 服务商并填写根域名"))
		return
	}
	var ids []string
	s.st.DB.Model(&store.Node{}).Order("sort_order, name").Pluck("id", &ids)
	type result struct {
		NodeID string `json:"nodeId"`
		Domain string `json:"domain"`
		Error  string `json:"error,omitempty"`
	}
	out := []result{}
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
		b, err := s.autoBindNode(ctx, id)
		cancel()
		if b == nil && err == nil {
			continue // already bound
		}
		r := result{NodeID: id}
		if b != nil {
			r.Domain = b.Domain
		}
		if err != nil {
			r.Error = err.Error()
		}
		out = append(out, r)
	}
	s.auditCtx(c, "domain.auto_bind_all", fmt.Sprintf("%d nodes", len(out)), "")
	c.JSON(http.StatusOK, out)
}

// syncAllBindingsAPI is POST /domains/sync-all: rewrite every binding now
// and report each one's outcome.
func (s *Server) syncAllBindingsAPI(c *gin.Context) {
	var bs []store.DomainBinding
	s.st.DB.Order("domain").Find(&bs)
	type result struct {
		Domain string `json:"domain"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	out := make([]result, 0, len(bs))
	for i := range bs {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		err := s.syncBinding(ctx, &bs[i], true)
		cancel()
		r := result{Domain: bs[i].Domain, Status: bs[i].Status}
		if err != nil {
			r.Error = err.Error()
		} else if bs[i].Status == "pending" {
			r.Error = bs[i].LastError
		}
		out = append(out, r)
	}
	s.auditCtx(c, "domain.sync_all", fmt.Sprintf("%d bindings", len(out)), "")
	c.JSON(http.StatusOK, out)
}
