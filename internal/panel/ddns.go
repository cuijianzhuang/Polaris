package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Domain bindings point DNS names at nodes. With DDNS enabled on a node the
// panel follows the public IP the agent reports and rewrites every binding,
// the node address used in subscriptions, and upstream relay targets.

// dnsWriter writes one record set for a binding.
type dnsWriter interface {
	Set(ctx context.Context, domain, typ, content string, proxied bool, ttl int) error
	Delete(ctx context.Context, domain, typ string) error
}

type cfWriter struct {
	cf    *cfClient
	zones map[string]string
}

func (w *cfWriter) zone(ctx context.Context, domain string) (string, error) {
	if z, ok := w.zones[domain]; ok {
		return z, nil
	}
	z, err := w.cf.zoneFor(ctx, domain)
	if err == nil {
		w.zones[domain] = z
	}
	return z, err
}

func (w *cfWriter) Set(ctx context.Context, domain, typ, content string, proxied bool, ttl int) error {
	z, err := w.zone(ctx, domain)
	if err != nil {
		return err
	}
	return w.cf.setRecord(ctx, z, domain, typ, content, proxied, ttl)
}

func (w *cfWriter) Delete(ctx context.Context, domain, typ string) error {
	z, err := w.zone(ctx, domain)
	if err != nil {
		return err
	}
	return w.cf.deleteRecords(ctx, z, domain, typ)
}

// webhookWriter POSTs {"domain","type","content","proxied","ttl","action"} to a URL,
// for DNS providers without a built-in client.
type webhookWriter struct{ url, token string }

func (w *webhookWriter) post(ctx context.Context, body map[string]any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: %s", resp.Status)
	}
	return nil
}

func (w *webhookWriter) Set(ctx context.Context, domain, typ, content string, proxied bool, ttl int) error {
	return w.post(ctx, map[string]any{"action": "set", "domain": domain, "type": typ, "content": content, "proxied": proxied, "ttl": ttl})
}

func (w *webhookWriter) Delete(ctx context.Context, domain, typ string) error {
	return w.post(ctx, map[string]any{"action": "delete", "domain": domain, "type": typ})
}

func (s *Server) dnsWriterFor(providerID string) (dnsWriter, error) {
	var p store.DNSProvider
	if err := s.st.DB.First(&p, "id = ?", providerID).Error; err != nil {
		return nil, fmt.Errorf("DNS provider: %w", err)
	}
	tok := s.st.Box.MustOpen(p.Token)
	switch p.Kind {
	case "cloudflare":
		if tok == "" {
			return nil, fmt.Errorf("provider %s has no API token", p.Name)
		}
		return &cfWriter{cf: newCF(tok), zones: map[string]string{}}, nil
	case "webhook":
		return &webhookWriter{url: p.Endpoint, token: tok}, nil
	}
	return nil, fmt.Errorf("unknown DNS provider kind %q", p.Kind)
}

// bindingTargets returns the A / AAAA contents a binding should hold.
func bindingTargets(n *store.Node, family string) (v4, v6 string) {
	v4, v6 = n.PublicIPv4, n.PublicIPv6
	if ip := net.ParseIP(n.Address); ip != nil {
		if ip.To4() != nil {
			v4 = ip.String()
		} else {
			v6 = ip.String()
		}
	}
	switch family {
	case "ipv6":
		v4 = ""
	case "dual":
	default:
		v6 = ""
	}
	return
}

// syncBinding writes the binding's records if they differ from what was last written.
func (s *Server) syncBinding(ctx context.Context, b *store.DomainBinding, force bool) error {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", b.NodeID).Error; err != nil {
		return err
	}
	v4, v6 := bindingTargets(&n, b.Family)
	want := strings.Trim(v4+","+v6, ",")
	if want == "" {
		return s.markBinding(b, "pending", "节点暂无可用公网 IP")
	}
	if !force && b.Current == want && b.Status == "synced" {
		return nil
	}
	w, err := s.dnsWriterFor(b.ProviderID)
	if err != nil {
		return s.markBinding(b, "error", err.Error())
	}
	for _, rec := range []struct{ typ, content string }{{"A", v4}, {"AAAA", v6}} {
		if rec.content != "" {
			err = w.Set(ctx, b.Domain, rec.typ, rec.content, b.Proxied, b.TTL)
		} else if b.Family == "ipv4" && rec.typ == "AAAA" || b.Family == "ipv6" && rec.typ == "A" {
			continue // the other family is not managed by this binding
		} else {
			err = w.Delete(ctx, b.Domain, rec.typ)
		}
		if err != nil {
			return s.markBinding(b, "error", err.Error())
		}
	}
	now := time.Now()
	b.Current, b.SyncedAt = want, &now
	s.st.DB.Model(b).Updates(map[string]any{"current": want, "synced_at": now})
	s.audit("system", "ddns.sync", b.Domain, want, "")
	return s.markBinding(b, "synced", "")
}

func (s *Server) markBinding(b *store.DomainBinding, status, msg string) error {
	b.Status, b.LastError = status, truncate(msg, 1900)
	s.st.DB.Model(b).Updates(map[string]any{"status": status, "last_error": b.LastError})
	if status == "error" {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func (s *Server) syncNodeBindings(nodeID string, force bool) {
	var bs []store.DomainBinding
	s.st.DB.Where("node_id = ?", nodeID).Find(&bs)
	for i := range bs {
		ctx, cancel := context.WithTimeout(s.ctxBG(), 60*time.Second)
		if err := s.syncBinding(ctx, &bs[i], force); err != nil {
			s.log.Warn("ddns sync failed", "domain", bs[i].Domain, "err", err)
			s.notify(Event{Kind: "ddns_failed", NodeID: nodeID, Text: fmt.Sprintf("DDNS 更新失败：%s — %v", bs[i].Domain, err)})
		}
		cancel()
	}
}

// syncAllBindings is the periodic safety net.
func (s *Server) syncAllBindings() {
	var ids []string
	s.st.DB.Model(&store.DomainBinding{}).Distinct().Pluck("node_id", &ids)
	for _, id := range ids {
		s.syncNodeBindings(id, false)
	}
}

// observePublicIP records the agent-reported addresses and drives DDNS.
func (s *Server) observePublicIP(n *store.Node, v4, v6 string) {
	if (v4 == "" || v4 == n.PublicIPv4) && (v6 == "" || v6 == n.PublicIPv6) {
		return
	}
	upd := map[string]any{"ip_changed_at": time.Now()}
	if v4 != "" {
		upd["public_ipv4"] = v4
	}
	if v6 != "" {
		upd["public_ipv6"] = v6
	}
	old := n.Address
	addrChanged := false
	if n.DDNS {
		next := v4
		if next == "" {
			next = v6
		}
		if next != "" && next != n.Address {
			upd["address"] = next
			addrChanged = true
		}
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(upd)
	if n.PublicIPv4 == "" && n.PublicIPv6 == "" && !addrChanged {
		go s.syncNodeBindings(n.ID, false) // first report: fill pending bindings
		return
	}
	s.audit("system", "node.ip_changed", n.Name, fmt.Sprintf("v4 %s→%s v6 %s→%s", n.PublicIPv4, v4, n.PublicIPv6, v6), "")
	if addrChanged {
		var ids []string
		s.st.DB.Model(&store.Inbound{}).Where("node_id = ?", n.ID).Pluck("id", &ids)
		s.MarkDirty("node IP changed (DDNS)", s.nodesForInbounds(ids...)...)
		s.notify(Event{Kind: "ip_changed", NodeID: n.ID, Text: fmt.Sprintf("🔁 节点 %s IP 变更：%s → %s，已更新订阅与域名", n.Name, old, upd["address"])})
	}
	go s.syncNodeBindings(n.ID, false)
}

// --- API ---

func (s *Server) listDNSProviders(c *gin.Context) {
	var ps []store.DNSProvider
	s.st.DB.Order("name").Find(&ps)
	for i := range ps {
		ps[i].HasToken = ps[i].Token != ""
	}
	c.JSON(http.StatusOK, ps)
}

func (s *Server) saveDNSProvider(c *gin.Context) {
	var req struct {
		Name     string `json:"name" binding:"required"`
		Kind     string `json:"kind" binding:"required"`
		Token    string `json:"token"`
		Endpoint string `json:"endpoint"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Kind != "cloudflare" && req.Kind != "webhook" {
		badRequest(c, fmt.Errorf("kind must be cloudflare or webhook"))
		return
	}
	p := store.DNSProvider{ID: store.NewID()}
	if id := c.Param("id"); id != "" {
		if err := s.st.DB.First(&p, "id = ?", id).Error; err != nil {
			dbError(c, err)
			return
		}
	}
	p.Name, p.Kind, p.Endpoint = req.Name, req.Kind, req.Endpoint
	if req.Token != "" {
		p.Token = s.st.Box.Seal(req.Token)
	}
	if err := s.st.DB.Save(&p).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "dns_provider.save", p.Name, p.Kind)
	p.HasToken = p.Token != ""
	c.JSON(http.StatusOK, p)
}

func (s *Server) deleteDNSProvider(c *gin.Context) {
	var n int64
	s.st.DB.Model(&store.DomainBinding{}).Where("provider_id = ?", c.Param("id")).Count(&n)
	if n > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("仍有 %d 个域名绑定使用该服务商", n)})
		return
	}
	s.st.DB.Delete(&store.DNSProvider{}, "id = ?", c.Param("id"))
	s.auditCtx(c, "dns_provider.delete", c.Param("id"), "")
	c.Status(http.StatusNoContent)
}

func (s *Server) listBindings(c *gin.Context) {
	q := s.st.DB.Order("domain")
	if id := c.Query("node"); id != "" {
		q = q.Where("node_id = ?", id)
	}
	var bs []store.DomainBinding
	q.Find(&bs)
	c.JSON(http.StatusOK, bs)
}

func (s *Server) saveBinding(c *gin.Context) {
	var req struct {
		NodeID     string `json:"nodeId" binding:"required"`
		ProviderID string `json:"providerId" binding:"required"`
		Domain     string `json:"domain" binding:"required"`
		Family     string `json:"family"`
		Proxied    bool   `json:"proxied"`
		TTL        int    `json:"ttl"`
		SetDomain  bool   `json:"setNodeDomain"` // also use as the node's domain (ACME / {node.domain})
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	b := store.DomainBinding{ID: store.NewID(), Status: "pending"}
	if id := c.Param("id"); id != "" {
		if err := s.st.DB.First(&b, "id = ?", id).Error; err != nil {
			dbError(c, err)
			return
		}
	}
	if req.Family == "" {
		req.Family = "ipv4"
	}
	b.NodeID, b.ProviderID, b.Domain, b.Family, b.Proxied, b.TTL = req.NodeID, req.ProviderID, strings.ToLower(strings.TrimSuffix(req.Domain, ".")), req.Family, req.Proxied, req.TTL
	b.Current = "" // force a rewrite
	if err := s.st.DB.Save(&b).Error; err != nil {
		dbError(c, err)
		return
	}
	if req.SetDomain {
		s.st.DB.Model(&store.Node{}).Where("id = ?", b.NodeID).Update("domain", b.Domain)
		s.MarkDirty("node domain bound", b.NodeID)
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	err := s.syncBinding(ctx, &b, true)
	s.auditCtx(c, "domain.bind", b.Domain, b.NodeID)
	resp := gin.H{"binding": b}
	if err != nil {
		resp["warning"] = err.Error()
	}
	c.JSON(http.StatusOK, resp)
}

func (s *Server) syncBindingAPI(c *gin.Context) {
	var b store.DomainBinding
	if err := s.st.DB.First(&b, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	if err := s.syncBinding(ctx, &b, true); err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, b)
}

func (s *Server) deleteBinding(c *gin.Context) {
	var b store.DomainBinding
	if err := s.st.DB.First(&b, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if c.Query("purge") == "1" {
		if w, err := s.dnsWriterFor(b.ProviderID); err == nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
			_ = w.Delete(ctx, b.Domain, "A")
			_ = w.Delete(ctx, b.Domain, "AAAA")
			cancel()
		}
	}
	s.st.DB.Delete(&b)
	s.auditCtx(c, "domain.unbind", b.Domain, "")
	c.Status(http.StatusNoContent)
}
