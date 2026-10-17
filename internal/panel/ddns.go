package panel

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// DNS providers (also used by CDN profiles) and the public IP nodes report.
// Domain bindings and DDNS are a paid feature (ddns_pro.go in polaris-pro).

// observePublicIP records the agent-reported addresses; DDNS (a paid
// feature) then moves the node address and its domain bindings along.
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
	s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(upd)
	first := n.PublicIPv4 == "" && n.PublicIPv6 == ""
	if !first {
		s.audit("system", "node.ip_changed", n.Name, fmt.Sprintf("v4 %s→%s v6 %s→%s", n.PublicIPv4, v4, n.PublicIPv6, v6), "")
	}
	s.ddnsIPChanged(n, v4, v6, first)
}

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
