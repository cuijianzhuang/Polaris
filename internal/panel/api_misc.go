package panel

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// --- certificates ---

func (s *Server) listCerts(c *gin.Context) {
	var cs []store.Certificate
	s.st.DB.Order("domain").Find(&cs)
	type view struct {
		store.Certificate
		UsedBy int `json:"usedBy"`
	}
	out := make([]view, 0, len(cs))
	for _, x := range cs {
		out = append(out, view{Certificate: x, UsedBy: len(s.nodesUsingCert(x.ID))})
	}
	c.JSON(http.StatusOK, out)
}

// createCert uploads a PEM pair (manual), issues self-signed, or requests ACME.
func (s *Server) createCert(c *gin.Context) {
	var req struct {
		Domain   string `json:"domain" binding:"required"`
		Provider string `json:"provider"` // manual / selfsigned / acme
		Cert     string `json:"cert"`
		Key      string `json:"key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var out *store.Certificate
	var err error
	switch req.Provider {
	case "manual":
		if _, err := tls.X509KeyPair([]byte(req.Cert), []byte(req.Key)); err != nil {
			badRequest(c, fmt.Errorf("invalid certificate/key: %w", err))
			return
		}
		exp, _ := pki.ParseCertExpiry([]byte(req.Cert))
		out = &store.Certificate{ID: store.NewID(), Domain: req.Domain, Provider: "manual", Cert: req.Cert,
			Key: s.st.Box.Seal(req.Key), ExpireAt: &exp}
		err = s.st.DB.Create(out).Error
	case "acme":
		out, err = s.issueACME(req.Domain, nil)
	default:
		out, err = s.issueSelfSigned(req.Domain)
	}
	if err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "cert.create", req.Domain, out.Provider)
	c.JSON(http.StatusCreated, out)
}

func (s *Server) deleteCert(c *gin.Context) {
	id := c.Param("id")
	if n := len(s.nodesUsingCert(id)); n > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "证书仍被入站使用"})
		return
	}
	s.st.DB.Delete(&store.Certificate{}, "id = ?", id)
	s.auditCtx(c, "cert.delete", id, "")
	c.Status(http.StatusNoContent)
}

func (s *Server) renewCert(c *gin.Context) {
	var cert store.Certificate
	if err := s.st.DB.First(&cert, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if err := s.renewCertificate(&cert); err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, cert)
}

// --- settings ---

var secretSettings = map[string]bool{"acme_cf_token": true}

var editableSettings = map[string]bool{
	"site_title": true, "sub_name_template": true, "sub_update_interval": true, "ip_ban_minutes": true,
	"acme_email": true, "acme_cf_token": true, "acme_directory": true, "default_xray": true, "default_singbox": true,
}

func (s *Server) getSettings(c *gin.Context) {
	var all []store.Setting
	s.st.DB.Find(&all)
	out := map[string]any{}
	for _, st := range all {
		if !editableSettings[st.Key] {
			continue
		}
		if secretSettings[st.Key] {
			out[st.Key+"_set"] = st.Value != ""
			continue
		}
		out[st.Key] = st.Value
	}
	out["admin_path"] = s.cfg.AdminPath
	out["public_url"] = s.cfg.PublicURL
	out["agent_url"] = s.agentURL()
	out["ca_sha256"] = s.ca.Fingerprint()
	out["version"] = Version
	out["acme_ready"] = s.acmeConfigured()
	c.JSON(http.StatusOK, out)
}

func (s *Server) putSettings(c *gin.Context) {
	var req map[string]string
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var keys []string
	for k, v := range req {
		if !editableSettings[k] {
			continue
		}
		if secretSettings[k] {
			if v == "" {
				continue // empty means "unchanged"
			}
			v = s.st.Box.Seal(v)
		}
		if k == "sub_update_interval" || k == "ip_ban_minutes" {
			if _, err := strconv.Atoi(v); err != nil {
				badRequest(c, fmt.Errorf("%s must be a number", k))
				return
			}
		}
		if err := s.st.SetSetting(k, v); err != nil {
			serverError(c, err)
			return
		}
		keys = append(keys, k)
	}
	s.auditCtx(c, "settings.update", strings.Join(keys, ","), "")
	s.getSettings(c)
}

// --- subscription rule templates ---

func (s *Server) listSubTemplates(c *gin.Context) {
	var ts []store.SubTemplate
	s.st.DB.Order("format, name").Find(&ts)
	c.JSON(http.StatusOK, ts)
}

func (s *Server) updateSubTemplate(c *gin.Context) {
	var t store.SubTemplate
	if err := s.st.DB.First(&t, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req struct {
		Content string `json:"content"`
		Reset   bool   `json:"reset"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Reset {
		switch t.Format {
		case "clash":
			req.Content = sub.DefaultClashTemplate
		case "singbox":
			req.Content = sub.DefaultSingboxTemplate
		case "surge":
			req.Content = sub.DefaultSurgeTemplate
		}
	}
	// Validate by rendering with a sample node.
	if r, ok := sub.Get(t.Format); ok {
		sample := []sub.Node{{Name: "sample", Server: "203.0.113.1", Port: 443, Protocol: "trojan", Password: "x"}}
		if _, err := r.Render(sample, sub.Info{URL: "https://example.com/sub/x"}, req.Content); err != nil {
			badRequest(c, fmt.Errorf("模板无效: %w", err))
			return
		}
	}
	t.Content, t.UpdatedAt = req.Content, time.Now()
	s.st.DB.Save(&t)
	s.auditCtx(c, "sub_template.update", t.Format, "")
	c.JSON(http.StatusOK, t)
}

// --- audit ---

func (s *Server) listAudit(c *gin.Context) {
	q := s.st.DB.Order("id desc")
	if v := c.Query("action"); v != "" {
		q = q.Where("action LIKE ?", v+"%")
	}
	if v := c.Query("q"); v != "" {
		like := "%" + v + "%"
		q = q.Where("target LIKE ? OR detail LIKE ? OR admin LIKE ?", like, like, like)
	}
	var total int64
	q.Model(&store.AuditLog{}).Count(&total)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var logs []store.AuditLog
	q.Limit(limit).Offset(offset).Find(&logs)
	c.JSON(http.StatusOK, gin.H{"items": logs, "total": total})
}
