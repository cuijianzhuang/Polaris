package panel

import (
	"crypto/tls"
	"encoding/json"
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
		Domain    string `json:"domain" binding:"required"`
		Provider  string `json:"provider"`  // manual / selfsigned / acme
		Challenge string `json:"challenge"` // acme: "" (auto) / dns / http
		NodeID    string `json:"nodeId"`    // acme http: node that answers
		Cert      string `json:"cert"`
		Key       string `json:"key"`
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
		out, err = s.issueACME(req.Domain, nil, acmeOpts{Challenge: req.Challenge, NodeID: req.NodeID})
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

var secretSettings = map[string]bool{"acme_cf_token": true, "tg_token": true, "github_client_secret": true}

// sudoSettings change who can sign in, or hold credentials: saving them
// needs a recent re-verification (secrets only when a new value is set).
var sudoSettings = map[string]bool{"login_allowlist": true, "login_ban_failures": true, "github_client_id": true}

var editableSettings = map[string]bool{
	"site_title": true, "reality_scan_candidates": true, "expire_warn_days": true, "sub_name_template": true, "sub_update_interval": true, "ip_ban_minutes": true,
	"acme_email": true, "acme_cf_token": true, "acme_directory": true, "acme_http_port": true, "github_client_id": true, "github_client_secret": true, "default_xray": true, "default_singbox": true,
	"converter_mode": true, "converter_backend": true, "converter_config": true, "converter_params": true,
	"converter_for_users": true, "sub_operators": true, "sub_apply_ops_users": true, "allow_private_sources": true,
	"tg_enabled": true, "tg_token": true, "tg_api_base": true, "tg_notify_users": true,
	"backup_schedule": true, "backup_hour": true, "backup_keep": true, "backup_tg": true,
	"remind_expire_days": true, "remind_usage_pct": true, "tg_daily_report": true, "tg_daily_report_hour": true,
	"sub_enabled": true, "sub_base_url": true, "sub_title": true, "sub_support_url": true, "sub_show_info": true, "sub_announce": true,
	"sub_routing_enabled": true, "sub_routing_rules": true, "sub_show_bot": true, "portal_enabled": true, "sub_show_rate": true, "traffic_spike_factor": true, "traffic_spike_min_gb": true, "login_allowlist": true, "login_ban_failures": true,
	"sub_scheme": true, "github_mirror": true, "sub_skip_cert": true, "sub_force_udp": true,
	"geodata_cron": true, "geodata_outbound": true, "geodata_assets": true, "ip_limit_trusted": true,
	"auto_update": true, "auto_update_hour": true,
	"ddns_auto_provider": true, "ddns_auto_zone": true, "ddns_auto_family": true, "ddns_auto_address": true,
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
	out["acme_http_ready"] = s.acmeHTTPReady()
	out["portal_url"] = s.portalURL()
	c.JSON(http.StatusOK, out)
}

func (s *Server) putSettings(c *gin.Context) {
	var req map[string]string
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	for k, v := range req {
		if editableSettings[k] && (sudoSettings[k] || secretSettings[k] && v != "") && !s.sudoOK(c) {
			if cur := s.st.GetSetting(k, ""); sudoSettings[k] && cur == v {
				continue // unchanged: the settings form sends every field
			}
			sudoRequired(c)
			return
		}
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
		if k == "sub_operators" {
			var ops []sub.Operator
			if err := json.Unmarshal([]byte(v), &ops); err != nil {
				badRequest(c, fmt.Errorf("sub_operators: %w", err))
				return
			}
			if err := sub.ValidateChain(ops); err != nil {
				badRequest(c, err)
				return
			}
		}
		if err := validateGeodataSetting(k, v); err != nil {
			badRequest(c, err)
			return
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
	for _, k := range keys {
		if geodataSettings[k] { // part of every node's spec
			var ids []string
			s.st.DB.Model(&store.Node{}).Pluck("id", &ids)
			s.MarkDirty("geodata settings", ids...)
			break
		}
	}
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
		Content   string `json:"content"`
		Reset     bool   `json:"reset"`
		Name      string `json:"name"`
		IsDefault bool   `json:"isDefault"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Reset {
		req.Content = defaultTemplate(t.Format)
	} else if req.Content == "" {
		req.Content = t.Content // name / default-flag only
	}
	if req.Name != "" {
		t.Name = req.Name
	}
	if req.IsDefault {
		s.st.DB.Model(&store.SubTemplate{}).Where("format = ?", t.Format).Update("is_default", false)
		t.IsDefault = true
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

func defaultTemplate(format string) string {
	switch format {
	case "clash":
		return sub.DefaultClashTemplate
	case "singbox":
		return sub.DefaultSingboxTemplate
	case "surge":
		return sub.DefaultSurgeTemplate
	case "loon":
		return sub.DefaultLoonTemplate
	case "quanx":
		return sub.DefaultQuanxTemplate
	}
	return ""
}

// createSubTemplate adds another named rule template for a format (starting
// from the built-in default unless content is given).
func (s *Server) createSubTemplate(c *gin.Context) {
	var req struct {
		Format  string `json:"format" binding:"required"`
		Name    string `json:"name" binding:"required"`
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if defaultTemplate(req.Format) == "" {
		badRequest(c, fmt.Errorf("format %s has no templates", req.Format))
		return
	}
	t := store.SubTemplate{ID: store.NewID(), Format: req.Format, Name: req.Name, Content: orDefault(req.Content, defaultTemplate(req.Format)), UpdatedAt: time.Now()}
	if r, ok := sub.Get(t.Format); ok {
		if _, err := r.Render(nil, sub.Info{}, t.Content); err != nil {
			badRequest(c, fmt.Errorf("模板无效: %w", err))
			return
		}
	}
	if err := s.st.DB.Create(&t).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "sub_template.create", t.Format+"/"+t.Name, "")
	c.JSON(http.StatusCreated, t)
}

func (s *Server) deleteSubTemplate(c *gin.Context) {
	var t store.SubTemplate
	if err := s.st.DB.First(&t, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if t.IsDefault {
		c.JSON(http.StatusConflict, gin.H{"error": "默认模板不能删除，请先把其他模板设为默认"})
		return
	}
	s.st.DB.Delete(&t)
	s.auditCtx(c, "sub_template.delete", t.Format+"/"+t.Name, "")
	c.Status(http.StatusNoContent)
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
