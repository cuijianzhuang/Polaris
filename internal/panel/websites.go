package panel

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

var errWebsiteVersion = errors.New("网站已被其他窗口修改，请刷新后重试")

type websiteRequest struct {
	store.Website
	NodePaths map[string]string `json:"nodePaths"`
}
type websiteRoute struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Upstream  string `json:"upstream"`
	InboundID string `json:"inboundId,omitempty"`
}
type websitePreview struct {
	Routes    []websiteRoute `json:"routes"`
	Warnings  []string       `json:"warnings"`
	Errors    []string       `json:"errors"`
	Digest    string         `json:"digest"`
	FileCount int            `json:"fileCount"`
	Size      int            `json:"size"`
	PortState string         `json:"portState"`
}

func websiteDigest(w store.Website) string {
	b, _ := json.Marshal(w.Files.V)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func websiteView(w store.Website) gin.H {
	size := 0
	for _, b := range w.Files.V {
		size += len(b)
	}
	count := len(w.Files.V)
	digest := websiteDigest(w)
	w.Files = store.NewJSON[map[string][]byte](nil)
	return gin.H{"site": w, "fileCount": count, "size": size, "digest": digest}
}
func (s *Server) listWebsites(c *gin.Context) {
	var sites []store.Website
	if err := s.st.DB.Order("domain, id").Find(&sites).Error; err != nil {
		dbError(c, err)
		return
	}
	out := make([]gin.H, 0, len(sites))
	for _, w := range sites {
		out = append(out, websiteView(w))
	}
	c.JSON(200, gin.H{"sites": out, "adminPath": strings.TrimRight(s.cfg.AdminPath, "/"), "subPath": "/sub", "portalPath": portalPath})
}

// ZIP bytes never touch the host filesystem. Both expansion and JSON payloads
// are bounded before being admitted into the node's desired configuration.
func (s *Server) websiteArchive(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		badRequest(c, fmt.Errorf("ZIP 上传上限为 4 MiB"))
		return
	}
	z, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		badRequest(c, fmt.Errorf("请上传有效的 ZIP 静态网站包"))
		return
	}
	files := map[string][]byte{}
	size := 0
	if len(z.File) > model.MaxWebsiteFiles*2 {
		badRequest(c, fmt.Errorf("ZIP 文件数量过多"))
		return
	}
	for _, f := range z.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() {
			badRequest(c, fmt.Errorf("ZIP 不允许符号链接或特殊文件"))
			return
		}
		if _, ok := files[f.Name]; ok {
			badRequest(c, fmt.Errorf("ZIP 包含重复路径：%s", f.Name))
			return
		}
		if f.UncompressedSize64 > uint64(model.MaxWebsiteBytes-size) {
			badRequest(c, fmt.Errorf("静态网站解压后不能超过 8 MiB"))
			return
		}
		r, e := f.Open()
		if e != nil {
			badRequest(c, e)
			return
		}
		b, e := io.ReadAll(io.LimitReader(r, int64(model.MaxWebsiteBytes-size)+1))
		r.Close()
		if e != nil {
			badRequest(c, e)
			return
		}
		files[f.Name] = b
		size += len(b)
		if err = model.ValidateWebsiteFiles(files); err != nil {
			badRequest(c, err)
			return
		}
	}
	if _, ok := files["index.html"]; !ok {
		badRequest(c, fmt.Errorf("ZIP 根目录必须包含 index.html，请打包网站目录内的文件"))
		return
	}
	w := store.Website{Files: store.NewJSON(files)}
	c.JSON(200, gin.H{"files": files, "fileCount": len(files), "size": size, "digest": websiteDigest(w)})
}

func loadWebsiteRequest(c *gin.Context, db *gorm.DB) (websiteRequest, error) {
	var req websiteRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 12<<20)
	if err := c.ShouldBindJSON(&req); err != nil {
		return req, err
	}
	if req.ID != "" {
		var old store.Website
		if err := db.First(&old, "id = ?", req.ID).Error; err != nil {
			return req, err
		}
		if req.NodeID != old.NodeID {
			return req, fmt.Errorf("更换节点请创建独立网站入口")
		}
		if req.Files.V == nil {
			req.Files = old.Files
		}
	}
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))
	req.Port = 443
	req.Path = strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if req.Path == "" {
		req.Path = "/"
	}
	req.AdminPath = strings.TrimRight(req.AdminPath, "/")
	req.SubPath = strings.TrimRight(req.SubPath, "/")
	req.PortalPath = strings.TrimRight(req.PortalPath, "/")
	req.Upstream = strings.TrimRight(strings.TrimSpace(req.Upstream), "/")
	req.PanelUpstream = strings.TrimRight(strings.TrimSpace(req.PanelUpstream), "/")
	return req, nil
}

// Include public dependencies of the selected panel features, otherwise an
// installer, shop link or payment return would incorrectly reach the blog.
func websitePanelRoutes(w store.Website) []websiteRoute {
	if w.PanelUpstream == "" {
		return nil
	}
	out := []websiteRoute{}
	add := func(kind, path string) {
		if path != "" {
			out = append(out, websiteRoute{Kind: kind, Path: path, Upstream: w.PanelUpstream})
		}
	}
	add("admin", w.AdminPath)
	add("subscription", w.SubPath)
	add("portal", w.PortalPath)
	if w.AdminPath != "" {
		for _, p := range []string{"/install.sh", "/j", "/download"} {
			add("panel-service", p)
		}
	}
	if w.PortalPath != "" {
		for _, p := range []string{epayNotifyPath, epayReturnPath, statusPath} {
			add("portal", p)
		}
	}
	return out
}

func websiteOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// All database admission checks are repeated under the shared writer lock.
func (s *Server) validateWebsite(db *gorm.DB, req websiteRequest) (websitePreview, error) {
	p := websitePreview{Routes: []websiteRoute{}, Warnings: []string{}, Errors: []string{}, Digest: websiteDigest(req.Website), FileCount: len(req.Files.V), PortState: "unknown"}
	var node store.Node
	if err := db.First(&node, "id = ? AND revoked = ?", req.NodeID, false).Error; err != nil {
		return p, fmt.Errorf("请选择有效节点")
	}
	if req.Domain == "" || !domainRe.MatchString(req.Domain) || strings.Contains(req.Domain, "*") {
		return p, fmt.Errorf("请输入完整网站域名")
	}
	if !model.ValidWebsitePath(req.Path) {
		return p, fmt.Errorf("网站路径无效")
	}
	if err := model.ValidateWebsiteFiles(req.Files.V); err != nil {
		return p, err
	}
	for _, b := range req.Files.V {
		p.Size += len(b)
	}
	if req.Mode != "static" && req.Mode != "proxy" {
		return p, fmt.Errorf("网站模式应为 static 或 proxy")
	}
	if req.Mode == "static" {
		if _, ok := req.Files.V["index.html"]; !ok {
			return p, fmt.Errorf("请先上传根目录含 index.html 的静态网站 ZIP")
		}
	} else if err := model.ValidateWebsiteUpstream(req.Upstream, 443); err != nil {
		return p, err
	}
	var cert store.Certificate
	if db.First(&cert, "id = ?", req.CertificateID).Error != nil {
		return p, fmt.Errorf("请选择证书")
	}
	pair, err := tls.X509KeyPair([]byte(cert.Cert), []byte(s.st.Box.MustOpen(cert.Key)))
	if err != nil {
		return p, fmt.Errorf("证书或私钥无效")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return p, err
	}
	if err = leaf.VerifyHostname(req.Domain); err != nil {
		return p, fmt.Errorf("证书未覆盖网站域名 %s", req.Domain)
	}
	if time.Now().Before(leaf.NotBefore) || time.Now().After(leaf.NotAfter) {
		return p, fmt.Errorf("网站证书尚未生效或已过期")
	}
	if cert.Provider == "selfsigned" {
		p.Warnings = append(p.Warnings, "自签证书不被浏览器默认信任，请配置可信证书")
	}
	if cert.Provider == "cf-origin" {
		p.Warnings = append(p.Warnings, "Origin CA 证书用于 Cloudflare 回源，请开启代理和 Full (strict)")
	}
	var count int64
	if err = db.Model(&store.Website{}).Where("node_id = ? AND domain = ? AND id <> ?", req.NodeID, req.Domain, req.ID).Count(&count).Error; err != nil {
		return p, err
	}
	if count > 0 {
		return p, fmt.Errorf("该节点已配置此域名的网站入口")
	}
	var sites []store.Website
	if err = db.Where("node_id = ? AND enabled = ? AND id <> ?", req.NodeID, true, req.ID).Find(&sites).Error; err != nil {
		return p, err
	}
	if len(sites) >= 8 {
		return p, fmt.Errorf("每个节点最多启用 8 个网站入口")
	}
	if req.PanelUpstream != "" {
		if err = model.ValidateWebsiteUpstream(req.PanelUpstream, 443); err != nil {
			return p, err
		}
		if req.AdminPath != "" && req.AdminPath != strings.TrimRight(s.cfg.AdminPath, "/") {
			return p, fmt.Errorf("后台路径须与系统设置一致，请先在「面板访问」配置私密后台路径")
		}
		if req.SubPath != "" && req.SubPath != "/sub" {
			return p, fmt.Errorf("订阅路径须保留为 /sub")
		}
		if req.PortalPath != "" && req.PortalPath != portalPath {
			return p, fmt.Errorf("用户中心路径须保留为 %s", portalPath)
		}
		for _, r := range []struct{ kind, path string }{{"admin", req.AdminPath}, {"subscription", req.SubPath}, {"portal", req.PortalPath}} {
			if r.path == "" {
				continue
			}
			if !model.ValidWebsitePath(r.path) || r.path == "/" {
				return p, fmt.Errorf("后台、订阅和用户中心须使用独立非根路径")
			}

		}
	} else if req.AdminPath != "" || req.SubPath != "" || req.PortalPath != "" {
		return p, fmt.Errorf("启用面板路径前请填写目标节点上的本机面板地址")
	}
	p.Routes = append(p.Routes, websitePanelRoutes(req.Website)...)
	var ins []store.Inbound
	if err = db.Where("node_id = ? AND enabled = ?", req.NodeID, true).Order("tag").Find(&ins).Error; err != nil {
		return p, err
	}
	known := map[string]bool{}
	for _, in := range ins {
		fi, fronted := frontProfile(db, in.ID)
		m := s.toModelInbound(&in)
		if req.Enabled && in.Port == 443 && m.UsesTCP() && !(fronted && fi.Port == 443 && m.CDNCapable() && m.Security.Type == model.SecTLS) {
			return p, fmt.Errorf("443 已被入站「%s」占用", in.Tag)
		}
		if !fronted || fi.Port != 443 || fi.Domain != req.Domain || !m.CDNCapable() || m.Security.Type != model.SecTLS {
			continue
		}
		routePath := m.Transport.Path
		if m.Transport.Type == model.TransGRPC {
			routePath = "/" + m.Transport.ServiceName
		}
		if v, ok := req.NodePaths[in.ID]; ok {
			routePath = v
		}
		known[in.ID] = true
		if !model.ValidWebsitePath(routePath) || routePath == "/" {
			return p, fmt.Errorf("入站「%s」需要独立且有效的节点路径", in.Tag)
		}
		if m.Transport.Type == model.TransGRPC && strings.Contains(strings.TrimPrefix(routePath, "/"), "/") {
			return p, fmt.Errorf("gRPC 服务名不能包含斜杠")
		}
		// Endpoint path overrides must agree with the listener to avoid publishing a broken link.
		var eps []store.Endpoint
		if err = db.Where("inbound_id = ?", in.ID).Find(&eps).Error; err != nil {
			return p, err
		}
		oldPath := m.Transport.Path
		if m.Transport.Type == model.TransGRPC {
			oldPath = "/" + m.Transport.ServiceName
		}
		for _, ep := range eps {
			if ep.SNI != "" && leaf.VerifyHostname(ep.SNI) != nil {
				return p, fmt.Errorf("网站证书未覆盖入站「%s」的入口 SNI：%s", in.Tag, ep.SNI)
			}
			if ep.Path != "" && ep.Path != oldPath && ep.Path != routePath {
				return p, fmt.Errorf("入站「%s」有独立入口路径，请先在入站中统一", in.Tag)
			}
		}
		p.Routes = append(p.Routes, websiteRoute{Kind: "node", Path: routePath, Upstream: in.Tag, InboundID: in.ID})
	}
	for id := range req.NodePaths {
		if !known[id] {
			return p, fmt.Errorf("节点路径只能修改同域名、443 的已启用 Caddy 统一回源入站")
		}
	}
	for i, r := range p.Routes {
		for _, other := range p.Routes[:i] {
			if websiteOverlap(r.Path, other.Path) {
				return p, fmt.Errorf("路径冲突：%s 与 %s", r.Path, other.Path)
			}
		}
		if req.Path != "/" && websiteOverlap(req.Path, r.Path) {
			return p, fmt.Errorf("网站路径与入口冲突：%s", r.Path)
		}
	}
	var forwards []store.ForwardRule
	if err = db.Where("node_id = ? AND enabled = ? AND listen_port = ?", req.NodeID, true, 443).Find(&forwards).Error; err != nil {
		return p, err
	}
	for _, f := range forwards {
		if strings.Contains(f.Network, "tcp") || f.Network == "" {
			return p, fmt.Errorf("443 已被转发「%s」占用", f.Name)
		}
	}
	target := "静态网站"
	if req.Mode == "proxy" {
		target = req.Upstream
	}
	p.Routes = append(p.Routes, websiteRoute{Kind: "website", Path: req.Path, Upstream: target})
	if !contains(node.AgentFeatures.V, model.WebsiteFeature) {
		if req.Enabled {
			return p, fmt.Errorf("目标节点需先升级 Agent，支持统一网站入口后才能启用")
		}
		p.Warnings = append(p.Warnings, "目标节点需要升级 Agent，支持统一网站入口后才能应用")
	}
	if node.Status != "online" {
		p.Warnings = append(p.Warnings, "节点离线，无法核实系统端口占用；上线应用时会再次检查")
	}
	p.Warnings = append(p.Warnings, "节点路径来自同域名的 Caddy 统一回源入站；新增节点请先在入站中绑定对应 CDN 配置档")
	return p, nil
}

func (s *Server) websitePortState(ctx context.Context, req websiteRequest, p *websitePreview) {
	if s.hub == nil || !s.hub.Online(req.NodeID) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	res, err := s.hub.Command(ctx, req.NodeID, "busy_ports", nil, 8*time.Second)
	var ports struct {
		TCP []int `json:"tcp"`
	}
	if err != nil || !res.Ok || json.Unmarshal([]byte(res.Output), &ports) != nil {
		p.Warnings = append(p.Warnings, "无法获取节点端口状态，应用时由 Agent 再次检查")
		return
	}
	p.PortState = "free"
	if !slices.Contains(ports.TCP, 443) {
		return
	}
	var running store.Core
	s.st.DB.Where("node_id = ? AND type = ?", req.NodeID, model.CoreCaddy).First(&running)
	var spec *model.NodeSpec
	spec, _ = s.BuildSpec(req.NodeID)
	if running.Status == "running" && spec != nil && spec.HasWebFront() {
		p.PortState = "managed"
		return
	}
	p.PortState = "occupied"
	p.Errors = append(p.Errors, "节点 443 已被其他程序占用；请先处理 Nginx、宝塔或其他 Caddy 服务")
}
func (s *Server) previewWebsite(c *gin.Context) {
	req, err := loadWebsiteRequest(c, s.st.DB)
	if err != nil {
		badRequest(c, err)
		return
	}
	p, err := s.validateWebsite(s.st.DB, req)
	if err != nil {
		p.Errors = append(p.Errors, err.Error())
	}
	if err == nil {
		s.websitePortState(c.Request.Context(), req, &p)
	}
	c.JSON(200, p)
}
func (s *Server) saveWebsite(c *gin.Context) {
	req, err := loadWebsiteRequest(c, s.st.DB)
	if err != nil {
		badRequest(c, err)
		return
	}
	p, err := s.validateWebsite(s.st.DB, req)
	if err != nil {
		badRequest(c, err)
		return
	}
	s.websitePortState(c.Request.Context(), req, &p)
	if req.Enabled && len(p.Errors) > 0 {
		badRequest(c, errors.New(strings.Join(p.Errors, "；")))
		return
	}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		checked, err := s.validateWebsite(tx, req)
		if err != nil {
			return err
		}
		if req.NodePaths == nil {
			req.NodePaths = map[string]string{}
		}
		for _, r := range checked.Routes {
			if r.Kind == "node" {
				req.NodePaths[r.InboundID] = r.Path
			}
		}
		if req.ID == "" {
			req.ID = store.NewID()
			req.Version = 1
			return saveNewWebsite(tx, &req)
		}
		updates := map[string]any{"domain": req.Domain, "port": 443, "certificate_id": req.CertificateID, "enabled": req.Enabled, "path": req.Path, "mode": req.Mode, "upstream": req.Upstream, "panel_upstream": req.PanelUpstream, "admin_path": req.AdminPath, "sub_path": req.SubPath, "portal_path": req.PortalPath, "files": req.Files, "version": req.Version + 1, "updated_at": time.Now()}
		result := tx.Model(&store.Website{}).Where("id = ? AND version = ?", req.ID, req.Version).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errWebsiteVersion
		}
		return updateWebsiteNodePaths(tx, req.NodePaths, req.CertificateID)
	})
	if err != nil {
		if errors.Is(err, errWebsiteVersion) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		} else {
			badRequest(c, err)
		}
		return
	}
	if _, _, err := s.Publish(req.NodeID, "website configuration", false); err != nil {
		s.st.DB.Model(&store.Node{}).Where("id = ?", req.NodeID).Update("last_error", truncate("网站配置已保存，但发布失败："+err.Error(), 1900))
		s.MarkDirty("website publish retry", req.NodeID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "网站配置已保存，但发布失败，请刷新后查看节点错误：" + err.Error()})
		return
	}
	s.auditCtx(c, "website.save", req.ID, fmt.Sprintf("%s files=%d sha256=%s", req.Domain, p.FileCount, p.Digest))
	var saved store.Website
	s.st.DB.First(&saved, "id = ?", req.ID)
	c.JSON(200, websiteView(saved))
}
func saveNewWebsite(tx *gorm.DB, req *websiteRequest) error {
	if err := tx.Create(&req.Website).Error; err != nil {
		return err
	}
	return updateWebsiteNodePaths(tx, req.NodePaths, req.CertificateID)
}
func updateWebsiteNodePaths(tx *gorm.DB, paths map[string]string, certificateID string) error {
	for id, p := range paths {
		var in store.Inbound
		if err := tx.First(&in, "id = ?", id).Error; err != nil {
			return err
		}
		tr := in.Transport.V
		if tr.Type == model.TransGRPC {
			tr.ServiceName = strings.TrimPrefix(p, "/")
		} else {
			tr.Path = p
		}
		sec := in.Security.V
		sec.CertificateID = certificateID
		updates := map[string]any{"transport": store.NewJSON(tr), "security": store.NewJSON(sec)}
		if in.TemplateID != "" {
			before := model.Inbound{Transport: in.Transport.V, Security: in.Security.V}
			after := before
			after.Transport = tr
			after.Security = sec
			updates["overridden_fields"] = store.NewJSON(mergeFields(in.OverriddenFields.V, diffFields(&before, &after)))
		}
		if err := tx.Model(&in).Updates(updates).Error; err != nil {
			return err
		}
		if err := tx.Model(&store.Endpoint{}).Where("inbound_id = ? AND path <> ?", id, "").Update("path", p).Error; err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) deleteWebsite(c *gin.Context) {
	var w store.Website
	if err := s.st.DB.First(&w, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if err := s.st.DB.Delete(&w).Error; err != nil {
		dbError(c, err)
		return
	}
	if _, _, err := s.Publish(w.NodeID, "website deleted", false); err != nil {
		s.st.DB.Model(&store.Node{}).Where("id = ?", w.NodeID).Update("last_error", truncate("网站已删除，但发布失败："+err.Error(), 1900))
		s.MarkDirty("website deletion retry", w.NodeID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "网站已删除，但发布失败，请查看节点错误：" + err.Error()})
		return
	}
	s.auditCtx(c, "website.delete", w.ID, w.Domain)
	c.Status(204)
}
func (s *Server) buildWebsites(spec *model.NodeSpec) error {
	var sites []store.Website
	if err := s.st.DB.Where("node_id = ? AND enabled = ?", spec.NodeID, true).Order("domain, id").Find(&sites).Error; err != nil {
		return err
	}
	for _, w := range sites {
		if _, err := s.validateWebsite(s.st.DB, websiteRequest{Website: w}); err != nil {
			return fmt.Errorf("网站 %s：%w", w.Domain, err)
		}
		var cert store.Certificate
		if err := s.st.DB.First(&cert, "id = ?", w.CertificateID).Error; err != nil {
			return fmt.Errorf("网站 %s 的证书不存在", w.Domain)
		}
		site := model.WebsiteSpec{ID: w.ID, Domain: w.Domain, Port: 443, Path: w.Path, Mode: w.Mode, Upstream: w.Upstream, Files: w.Files.V, CertPEM: cert.Cert, KeyPEM: s.st.Box.MustOpen(cert.Key)}
		for _, r := range websitePanelRoutes(w) {
			site.Proxies = append(site.Proxies, model.WebsiteProxy{Path: r.Path, Upstream: r.Upstream})
		}
		spec.Websites = append(spec.Websites, site)
	}
	return nil
}
