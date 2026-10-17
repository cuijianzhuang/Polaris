package panel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Licensing on the panel side: the installed license (verified offline
// with the built-in public keys), online activation, a daily check with the
// license server, and the 30-day trial for installs that already used the
// paid features when they upgraded. Nothing here ever stops a running
// service: without a license, paid features cannot be changed, but what
// they already configured keeps working.

// BuildDate is the release date (YYYY-MM-DD), set at build time; a license's
// update period is checked against it.
var BuildDate = ""

// DefaultLicenseServer is used when neither the setting nor
// POLARIS_LICENSE_SERVER names one ("" = no online activation). Set at build
// time once the license server runs:
// -ldflags "-X github.com/cuijianzhuang/polaris/internal/panel.DefaultLicenseServer=https://…"
var DefaultLicenseServer = ""

type licenseState struct {
	st  license.State
	lic *license.License
}

var licClient = &http.Client{Timeout: 20 * time.Second}

func (s *Server) licenseKeys() []ed25519.PublicKey {
	if s.licKeys != nil {
		return s.licKeys
	}
	keys := license.ParseKeys(license.PublicKeys)
	if len(keys) == 0 {
		// Development builds: keys for testing come from the environment.
		keys = license.ParseKeys(os.Getenv("POLARIS_LICENSE_PUBKEYS"))
	}
	return keys
}

// instanceID is this panel's identity for licensing (random, kept in the
// database, so it moves with a backup).
func (s *Server) instanceID() string {
	if id := s.st.GetSetting("instance_id", ""); id != "" {
		return id
	}
	id := "inst_" + strings.ToLower(pki.Token(16))
	s.st.SetSetting("instance_id", id)
	return id
}

func (s *Server) licenseServer() string {
	if v := strings.TrimSpace(os.Getenv("POLARIS_LICENSE_SERVER")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return strings.TrimRight(s.st.GetSetting("license_server", DefaultLicenseServer), "/")
}

func settingTime(s *Server, key string) time.Time {
	t, _ := time.Parse(time.RFC3339, s.st.GetSetting(key, ""))
	return t
}

// initLicense runs at startup: instance id, the one-time trial grant, state.
func (s *Server) initLicense() {
	s.instanceID()
	// The trial starts with the first build that enforces licensing.
	if proBuild && len(s.licenseKeys()) > 0 && s.st.GetSetting("license_trial_checked", "") == "" {
		if s.usesPaidFeatures() {
			s.st.SetSetting("license_trial_until", time.Now().Add(30*24*time.Hour).Format(time.RFC3339))
			s.log.Info("paid features in use: 30-day trial started")
		}
		s.st.SetSetting("license_trial_checked", "1")
	}
	s.reloadLicense()
}

// usesPaidFeatures reports whether this install already relies on a paid
// feature (it gets the trial when upgrading).
func (s *Server) usesPaidFeatures() bool {
	var n int64
	s.st.DB.Model(&store.User{}).Where("speed_up_mbps > 0 OR speed_down_mbps > 0").Count(&n)
	if n == 0 {
		s.st.DB.Model(&store.Group{}).Where("speed_up_mbps > 0 OR speed_down_mbps > 0").Count(&n)
	}
	return n > 0 || len(s.pushTargets()) > 0 || s.st.GetSetting("tg_enabled", "") == "true"
}

// reloadLicense re-evaluates the license in effect from the settings.
func (s *Server) reloadLicense() {
	var lic *license.License
	if text := s.st.GetSetting("license_text", ""); text != "" {
		l, err := license.Parse(text, s.licenseKeys(), s.instanceID())
		if err != nil {
			s.log.Warn("installed license is not valid", "err", err)
		} else {
			lic = l
		}
	}
	released, _ := time.Parse("2006-01-02", BuildDate)
	st := license.Evaluate(license.Inputs{
		License: lic, Revoked: s.st.GetSetting("license_revoked", "") == "1",
		LastCheck: settingTime(s, "license_check_at"), TrialUntil: settingTime(s, "license_trial_until"),
		Released: released, Now: time.Now(), Unenforced: len(s.licenseKeys()) == 0,
	})
	if !proBuild {
		st = communityBuild(st)
	}
	s.lic.Store(&licenseState{st: st, lic: lic})
}

// Edition names this build: "pro" with the polaris-pro sources, else
// "community".
func Edition() string {
	if proBuild {
		return "pro"
	}
	return "community"
}

// editionAsset is the infix of this edition's release assets:
// polaris-agent-pro-linux-amd64, polaris-panel-pro-v1.2.3-linux-amd64.tar.gz.
func editionAsset() string {
	if proBuild {
		return "pro-"
	}
	return ""
}

// communityBuild: a build without the polaris-pro sources has none of the
// paid features, whatever the license says.
func communityBuild(st license.State) license.State {
	st.Features = []string{}
	st.TrialUntil = nil
	switch st.Plan {
	case "open", "trial":
		st.Plan, st.Source = "community", "none"
	}
	if st.Source == "license" {
		st.Warnings = append(st.Warnings, "当前是社区版构建（从源码编译），不包含高级功能；请使用官方发布版以启用许可证中的功能")
	}
	return st
}

// license is the state in effect.
func (s *Server) license() license.State {
	if p := s.lic.Load(); p != nil {
		return p.st
	}
	return license.Evaluate(license.Inputs{Now: time.Now()})
}

func (s *Server) installedLicense() *license.License {
	if p := s.lic.Load(); p != nil {
		return p.lic
	}
	return nil
}

var planNames = map[string]string{"pro": "专业版", "enterprise": "企业版"}

// requireFeature answers 402 when a paid feature is not licensed.
func (s *Server) requireFeature(c *gin.Context, f string) bool {
	st := s.license()
	if st.Allow(f) {
		return true
	}
	c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{"error": "此功能需要专业版许可证（系统 → 许可证）", "feature": f, "plan": "pro"})
	return false
}

// limitOK checks a node / user cap before adding n more.
func (s *Server) limitOK(c *gin.Context, kind string, adding int) bool {
	max := s.license().Limit(kind)
	if max <= 0 {
		return true
	}
	var cur int64
	switch kind {
	case license.LimitNodes:
		s.st.DB.Model(&store.Node{}).Count(&cur)
	case license.LimitUsers:
		s.st.DB.Model(&store.User{}).Where("is_service = ?", false).Count(&cur)
	}
	if int(cur)+adding <= max {
		return true
	}
	what := map[string]string{license.LimitNodes: "节点", license.LimitUsers: "用户"}[kind]
	c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
		"error": fmt.Sprintf("当前许可最多 %d 个%s（已有 %d 个），请升级许可证", max, what, cur), "limit": kind, "plan": "pro"})
	return false
}

// installLicense verifies and stores a license text.
func (s *Server) installLicense(text string) (*license.License, error) {
	l, err := license.Parse(text, s.licenseKeys(), s.instanceID())
	if err != nil {
		return nil, err
	}
	s.st.SetSetting("license_text", strings.Join(strings.Fields(text), ""))
	s.st.SetSetting("license_revoked", "")
	s.st.SetSetting("license_check_at", time.Now().Format(time.RFC3339))
	s.reloadLicense()
	return l, nil
}

// licenseCall posts to the license server.
func (s *Server) licenseCall(ctx context.Context, path string, body any, out any) error {
	base := s.licenseServer()
	if base == "" {
		return errors.New("未配置授权服务器（POLARIS_LICENSE_SERVER）")
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "polaris-panel/"+Version)
	resp, err := licClient.Do(req)
	if err != nil {
		return fmt.Errorf("连接授权服务器失败：%w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("授权服务器返回 %s", resp.Status)
	}
	return json.Unmarshal(data, out)
}

// activateLicense exchanges an activation key for a license bound to this
// instance.
func (s *Server) activateLicense(ctx context.Context, key string) (*license.License, error) {
	var out struct {
		License string `json:"license"`
	}
	if err := s.licenseCall(ctx, "/v1/activate", map[string]string{"key": strings.TrimSpace(key), "instance": s.instanceID(), "version": Version}, &out); err != nil {
		return nil, err
	}
	return s.installLicense(out.License)
}

// deactivateLicense releases the license from this instance (to move it).
// The local copy is removed even when the server cannot be reached; the
// binding can then be released in the license backend.
func (s *Server) deactivateLicense(ctx context.Context) error {
	var err error
	if l := s.installedLicense(); l != nil && !l.Offline {
		var out map[string]any
		err = s.licenseCall(ctx, "/v1/deactivate", map[string]string{"license_id": l.ID, "instance": s.instanceID()}, &out)
	}
	s.st.SetSetting("license_text", "")
	s.st.SetSetting("license_revoked", "")
	s.reloadLicense()
	return err
}

// checkLicense is the daily online check: revocation and renewals. A
// failed check only records nothing new; the state warns after the grace
// period.
func (s *Server) checkLicense() {
	l := s.installedLicense()
	if l == nil {
		if s.st.GetSetting("license_text", "") != "" {
			s.reloadLicense() // e.g. a license for a newer key set
		}
		return
	}
	if l.Offline {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out struct {
		OK      bool   `json:"ok"`
		Revoked bool   `json:"revoked"`
		License string `json:"license"` // a renewed license, if any
	}
	if err := s.licenseCall(ctx, "/v1/check", map[string]string{"license_id": l.ID, "instance": s.instanceID(), "version": Version}, &out); err != nil {
		s.log.Warn("license check failed", "err", err)
		s.reloadLicense()
		return
	}
	s.st.SetSetting("license_check_at", time.Now().Format(time.RFC3339))
	if out.Revoked {
		s.st.SetSetting("license_revoked", "1")
		s.notify(Event{Kind: "license", Text: "⚠️ 许可证 " + l.ID + " 已被吊销，面板已恢复为社区版（已生效的配置保持不变）"})
	} else {
		s.st.SetSetting("license_revoked", "")
		if out.License != "" {
			if _, err := s.installLicense(out.License); err != nil {
				s.log.Warn("renewed license rejected", "err", err)
			}
		}
	}
	s.reloadLicense()
}

// --- API ---

func (s *Server) licenseView() gin.H {
	st := s.license()
	var nodes, users int64
	s.st.DB.Model(&store.Node{}).Count(&nodes)
	s.st.DB.Model(&store.User{}).Where("is_service = ?", false).Count(&users)
	out := gin.H{"state": st, "instance": s.instanceID(), "usage": gin.H{"nodes": nodes, "users": users},
		"server": s.licenseServer() != "", "keys": len(s.licenseKeys()) > 0, "planName": planNames[st.Plan],
		"edition": Edition()}
	if l := s.installedLicense(); l != nil {
		out["license"] = gin.H{"id": l.ID, "customer": l.Customer, "plan": l.Plan, "offline": l.Offline,
			"issuedAt": l.IssuedAt, "expiresAt": l.ExpiresAt, "updatesUntil": l.UpdatesUntil, "features": l.Features, "limits": l.Limits}
	} else if s.st.GetSetting("license_text", "") != "" {
		out["invalid"] = true // installed but not valid for this build or instance
	}
	if t := settingTime(s, "license_check_at"); !t.IsZero() {
		out["checkedAt"] = t
	}
	return out
}

// GET /license
func (s *Server) getLicense(c *gin.Context) { c.JSON(http.StatusOK, s.licenseView()) }

// POST /license {text}
func (s *Server) postLicense(c *gin.Context) {
	var req struct {
		Text string `json:"text" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	l, err := s.installLicense(req.Text)
	if err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "license.install", l.ID, l.Plan)
	c.JSON(http.StatusOK, s.licenseView())
}

// POST /license/activate {key}
func (s *Server) activateLicenseAPI(c *gin.Context) {
	var req struct {
		Key string `json:"key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	l, err := s.activateLicense(c.Request.Context(), req.Key)
	if err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "license.activate", l.ID, l.Plan)
	c.JSON(http.StatusOK, s.licenseView())
}

// POST /license/deactivate
func (s *Server) deactivateLicenseAPI(c *gin.Context) {
	id := ""
	if l := s.installedLicense(); l != nil {
		id = l.ID
	}
	err := s.deactivateLicense(c.Request.Context())
	s.auditCtx(c, "license.deactivate", id, "")
	out := s.licenseView()
	if err != nil {
		out["warning"] = "本机已移除许可证，但未能通知授权服务器（" + err.Error() + "），如需迁移请在授权后台解绑"
	}
	c.JSON(http.StatusOK, out)
}
