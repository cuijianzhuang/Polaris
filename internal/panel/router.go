package panel

import (
	"bytes"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/cuijianzhuang/polaris/internal/store"
)

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("polaris-timing-guard"), bcrypt.DefaultCost)

func badRequest(c *gin.Context, err error) {
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}

func serverError(c *gin.Context, err error) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

func notFound(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "not found"})
}

func dbError(c *gin.Context, err error) {
	switch {
	case store.IsNotFound(err):
		notFound(c)
	case store.IsDuplicate(err):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "名称或标签已存在"})
	default:
		serverError(c, err)
	}
}

// auditCtx records a write performed by the current admin.
func (s *Server) auditCtx(c *gin.Context, action, target, detail string) {
	s.audit(c.GetString("admin"), action, target, detail, clientIP(c.Request))
}

// securityHeaders keeps secrets in URLs (admin path, subscription tokens) out
// of Referer headers and blocks framing / MIME sniffing.
func securityHeaders(c *gin.Context) {
	h := c.Writer.Header()
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	c.Next()
}

// Router builds the HTTP handler: admin UI + API under the admin path,
// public subscription/installer routes, and a decoy for everything else.
func (s *Server) Router() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), securityHeaders)
	r.RemoveExtraSlash = true

	// Public routes.
	r.GET("/sub/:token", s.subscription)
	r.GET("/sub/p/:token", s.profileSubscription)
	r.GET("/sub/cb/:token", s.subCallbackHandler)
	r.GET("/sub/scheme/:name", s.serveSchemeINI)
	r.GET("/sub/rs/:sig/:enc", s.serveRuleSet)
	r.GET("/install.sh", s.installScript)
	r.GET("/j/:token", s.joinScript)
	r.GET("/download/:file", s.download)

	admin := strings.TrimRight(s.cfg.AdminPath, "/")
	api := r.Group(s.cfg.APIBase())
	api.POST("/auth/login", s.login)
	api.POST("/auth/github/login-url", s.githubLoginURL)
	api.GET("/auth/github/callback", s.githubCallback)
	api.POST("/auth/github/totp", s.githubTOTP)
	api.GET("/info", s.publicInfo)
	api.POST("/auth/tg", s.tgLogin)
	api.POST("/auth/tg/start", s.tgWebLoginStart)
	api.POST("/auth/tg/poll", s.tgWebLoginPoll)

	p := api.Group("", s.authRequired, s.scopeGuard)
	p.GET("/ws", func(c *gin.Context) {
		s.handleWS(c.Writer, c.Request, s.sessionCheck(c), nodeScope(c))
	})
	p.GET("/openapi.json", func(c *gin.Context) { c.JSON(http.StatusOK, s.openAPI(r)) })
	p.GET("/auth/me", s.me)
	p.POST("/auth/password", s.changePassword)
	p.POST("/auth/totp/setup", s.totpSetup)
	p.POST("/auth/totp/enable", s.totpEnable)
	p.GET("/auth/github", s.githubInfo)
	p.POST("/auth/github/bind-url", s.githubBindURL)
	p.POST("/auth/github/unbind", s.githubUnbind)

	p.GET("/nodes", s.listNodes)
	p.POST("/nodes", s.createNode)
	p.GET("/nodes/:id", s.getNode)
	p.PUT("/nodes/:id", s.updateNode)
	p.DELETE("/nodes/:id", s.deleteNode)
	p.POST("/nodes/:id/install-token", s.installToken)
	p.POST("/nodes/:id/sync", s.syncNode)
	p.POST("/nodes/:id/geodata-update", s.geodataUpdate)
	p.POST("/tools/reality-scan", s.realityScan)
	p.GET("/tools/reality-candidates", s.realityCandidates)
	p.POST("/tools/vlessenc", s.genVLESSEnc)
	p.POST("/tools/mldsa65", s.genMLDSA65)
	p.POST("/nodes/:id/upgrade-agent", s.upgradeAgentAPI)
	p.GET("/nodes/:id/users", s.nodeUsersAPI)
	p.POST("/nodes/:id/users", s.bindNodeUsers)
	p.POST("/nodes/upgrade-agents", s.upgradeAllAgents)
	p.POST("/nodes/:id/rollback", s.rollbackNode)
	p.POST("/nodes/:id/revoke", s.revokeNode)
	p.GET("/nodes/:id/revisions", s.listRevisions)
	p.GET("/nodes/:id/spec", s.nodeSpec)
	p.GET("/nodes/:id/render", s.renderNodeCore)
	p.GET("/nodes/:id/cores", s.listCores)
	p.POST("/nodes/:id/cores", s.upsertCore)
	p.DELETE("/cores/:id", s.deleteCore)
	p.POST("/cores/:id/restart", s.restartCore)
	p.POST("/cores/:id/rollback", s.rollbackCore)
	p.POST("/cores/upgrade", s.upgradeCores)
	p.GET("/core-latest", s.coreLatest)
	p.GET("/core-versions", s.coreVersions)
	p.GET("/core-defaults", s.coreDefaults)
	p.GET("/cores/:id/logs", s.coreLogs)
	p.GET("/capabilities", s.capabilities)

	p.GET("/inbounds", s.listInbounds)
	p.POST("/inbounds", s.createInbound)
	p.POST("/inbounds/preview", s.previewDraftInbound)
	p.GET("/inbounds/:id", s.getInbound)
	p.PUT("/inbounds/:id", s.updateInbound)
	p.DELETE("/inbounds/:id", s.deleteInbound)
	p.GET("/inbounds/:id/preview", s.previewInbound)
	p.GET("/inbounds/:id/export", s.exportInbound)
	p.POST("/inbounds/:id/clone", s.cloneInbound)
	p.GET("/nodes/:id/free-port", s.freePort)
	p.GET("/forwards", s.listForwards)
	p.POST("/forwards", s.createForward)
	p.PUT("/forwards/:id", s.updateForward)
	p.DELETE("/forwards/:id", s.deleteForward)
	p.GET("/inbounds/:id/endpoints", s.listEndpoints)
	p.POST("/inbounds/:id/endpoints", s.createEndpoint)
	p.DELETE("/inbounds/:id/endpoints/:eid", s.deleteEndpoint)

	p.GET("/users", s.listUsers)
	p.POST("/users", s.createUser)
	p.POST("/users/bulk", s.bulkUsers)
	p.GET("/users/export", s.exportUsers)
	p.POST("/users/import", s.importUsers)
	p.PUT("/users/:id/traffic", s.setUserTraffic)
	p.GET("/users/:id/sub-check", s.subDiagnose)
	p.GET("/users/:id/devices", s.listDevices)
	p.DELETE("/users/:id/devices/:did", s.deleteDevice)
	p.GET("/users/:id", s.getUser)
	p.PUT("/users/:id", s.updateUser)
	p.DELETE("/users/:id", s.deleteUser)
	p.POST("/users/:id/reset-traffic", s.resetUserTraffic)
	p.POST("/users/:id/reset-token", s.resetUserToken)
	p.GET("/users/:id/links", s.userLinks)

	p.GET("/groups", s.listGroups)
	p.POST("/groups", s.createGroup)
	p.PUT("/groups/:id", s.updateGroup)
	p.DELETE("/groups/:id", s.deleteGroup)

	p.GET("/stats/overview", s.statsOverview)
	p.GET("/stats/traffic", s.statsTraffic)
	p.GET("/stats/tags", s.tagStats)
	p.GET("/stats/nodes", s.nodeTrafficAPI)
	p.POST("/stats/tags/reset", s.resetTagStats)

	p.GET("/certificates", s.listCerts)
	p.POST("/certificates", s.createCert)
	p.DELETE("/certificates/:id", s.deleteCert)
	p.POST("/certificates/:id/renew", s.renewCert)

	p.GET("/cdn-profiles", s.listCDN)
	p.POST("/cdn-profiles", s.createCDN)
	p.PUT("/cdn-profiles/:id", s.updateCDN)
	p.DELETE("/cdn-profiles/:id", s.deleteCDN)
	p.POST("/cdn-profiles/:id/sync-dns", s.syncCDNDNS)
	p.POST("/cdn-profiles/:id/refresh-ips", s.refreshCDNIPs)

	p.GET("/chains", s.listChains)
	p.POST("/chains", s.createChain)
	p.PUT("/chains/:id", s.updateChain)
	p.DELETE("/chains/:id", s.deleteChain)
	p.POST("/chains/:id/probe", s.probeChain)
	p.GET("/nodes/:id/outbounds", s.listOutbounds)
	p.POST("/nodes/:id/outbounds", s.createOutbound)
	p.PUT("/nodes/:id/outbounds/:oid", s.updateOutbound)
	p.DELETE("/nodes/:id/outbounds/:oid", s.deleteOutbound)
	p.GET("/routing/presets", s.listRulePresets)
	p.POST("/routing/presets/apply", s.applyRulePreset)
	p.GET("/nodes/:id/routing", s.getRouting)
	p.PUT("/nodes/:id/routing", s.putRouting)

	p.GET("/dns-providers", s.listDNSProviders)
	p.POST("/dns-providers", s.saveDNSProvider)
	p.PUT("/dns-providers/:id", s.saveDNSProvider)
	p.DELETE("/dns-providers/:id", s.deleteDNSProvider)
	p.GET("/domains", s.listBindings)
	p.POST("/domains", s.saveBinding)
	p.PUT("/domains/:id", s.saveBinding)
	p.POST("/domains/:id/sync", s.syncBindingAPI)
	p.DELETE("/domains/:id", s.deleteBinding)

	p.GET("/templates", s.listTemplates)
	p.POST("/templates", s.createTemplate)
	p.PUT("/templates/:id", s.updateTemplate)
	p.DELETE("/templates/:id", s.deleteTemplate)
	p.POST("/templates/:id/preview", s.previewTemplate)
	p.POST("/templates/:id/apply", s.applyTemplate)
	p.POST("/templates/:id/sync", s.syncTemplate)
	p.POST("/templates/:id/bulk", s.bulkTemplateInbounds)

	p.GET("/sub-templates", s.listSubTemplates)
	p.POST("/sub-templates", s.createSubTemplate)
	p.PUT("/sub-templates/:id", s.updateSubTemplate)
	p.DELETE("/sub-templates/:id", s.deleteSubTemplate)
	p.GET("/sub-meta", s.subMeta)
	p.GET("/sub-scheme-check", s.checkScheme)
	p.GET("/sub-sources", s.listSources)
	p.POST("/sub-sources", s.saveSource)
	p.PUT("/sub-sources/:id", s.saveSource)
	p.DELETE("/sub-sources/:id", s.deleteSource)
	p.POST("/sub-sources/:id/refresh", s.refreshSource)
	p.GET("/sub-sources/:id/nodes", s.sourceNodesAPI)
	p.GET("/sub-profiles", s.listProfiles)
	p.POST("/sub-profiles", s.saveProfile)
	p.POST("/sub-profiles/preview", s.previewProfile)
	p.PUT("/sub-profiles/:id", s.saveProfile)
	p.DELETE("/sub-profiles/:id", s.deleteProfile)
	p.POST("/sub-profiles/:id/reset-token", s.resetProfileToken)
	p.GET("/telegram", s.tgStatus)
	p.GET("/telegram/admins", s.listTGAdmins)
	p.POST("/telegram/bind-code", s.tgBindCode)
	p.PUT("/telegram/admins/:id", s.updateTGAdmin)
	p.DELETE("/telegram/admins/:id", s.deleteTGAdmin)
	p.POST("/telegram/test", s.tgTest)
	p.POST("/telegram/broadcast", s.tgBroadcast)
	p.GET("/backup", s.backupInfo)
	p.POST("/backup", s.backupCreate)
	p.POST("/backup/restore", s.backupRestore)
	p.GET("/backup/files/:name", s.backupDownload)
	p.DELETE("/backup/files/:name", s.backupDelete)
	p.POST("/backup/files/:name/restore", s.backupRestore)
	p.GET("/settings", s.getSettings)
	p.PUT("/settings", s.putSettings)
	p.GET("/audit-logs", s.listAudit)

	// Admin SPA under the admin path; everything else gets the decoy.
	var spa gin.HandlerFunc
	if s.webFS != nil {
		spa = s.spaHandler(admin)
	}
	decoy := s.decoy()
	apiPrefix := s.cfg.APIBase() + "/"
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		inAdmin := admin == "" || p == admin || strings.HasPrefix(p, admin+"/")
		switch {
		case strings.HasPrefix(p, apiPrefix):
			notFound(c)
		case inAdmin && spa != nil && c.Request.Method == http.MethodGet:
			spa(c)
		case admin == "":
			c.String(http.StatusNotFound, "404 page not found")
		default:
			decoy(c)
		}
	})
	return r
}

// spaHandler serves the embedded React build, falling back to index.html.
func (s *Server) spaHandler(prefix string) gin.HandlerFunc {
	fileServer := http.StripPrefix(prefix, http.FileServer(s.webFS))
	return func(c *gin.Context) {
		rel := strings.TrimPrefix(c.Request.URL.Path, prefix)
		if rel == "" || rel == "/" || path.Ext(rel) == "" {
			s.serveIndex(c, prefix)
			return
		}
		f, err := s.webFS.Open(rel)
		if err != nil {
			c.String(http.StatusNotFound, "404 page not found")
			return
		}
		f.Close()
		if strings.HasPrefix(rel, "/assets/") {
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(c.Writer, c.Request)
	}
}

func (s *Server) serveIndex(c *gin.Context, prefix string) {
	f, err := s.webFS.Open("/index.html")
	if err != nil {
		c.String(http.StatusOK, "Polaris API is running; frontend not built.")
		return
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	base := prefix + "/"
	inject := `<script>window.__POLARIS_BASE__=` + jsString(base) + `;</script>`
	b = bytes.Replace(b, []byte("<head>"), []byte("<head><base href=\""+html.EscapeString(base)+"\">"+inject), 1)
	c.Header("Cache-Control", "no-cache")
	c.Data(http.StatusOK, "text/html; charset=utf-8", b)
}

func jsString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "<", `\u003c`)
	return `"` + r.Replace(s) + `"`
}

// decoy answers unknown paths with 404 or a reverse-proxied camouflage site.
func (s *Server) decoy() gin.HandlerFunc {
	if u, err := url.Parse(s.cfg.Decoy); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		proxy := httputil.NewSingleHostReverseProxy(u)
		orig := proxy.Director
		proxy.Director = func(r *http.Request) {
			orig(r)
			r.Host = u.Host
		}
		return func(c *gin.Context) { proxy.ServeHTTP(c.Writer, c.Request) }
	}
	return func(c *gin.Context) { c.String(http.StatusNotFound, "404 page not found") }
}

func (s *Server) publicInfo(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"title": s.st.GetSetting("site_title", "Polaris"), "version": Version, "githubLogin": s.githubEnabled(), "telegramLogin": s.tgWebLoginEnabled()})
}

// download serves agent binaries placed in <data_dir>/downloads.
func (s *Server) download(c *gin.Context) {
	name := filepath.Base(c.Param("file"))
	if !strings.HasPrefix(name, "polaris-agent") && name != "install-panel.sh" { // agents, and the polaris tool for nodes
		notFound(c)
		return
	}
	p := filepath.Join(s.cfg.DataDir, "downloads", name)
	if _, err := os.Stat(p); err != nil {
		notFound(c)
		return
	}
	c.FileAttachment(p, name)
}

// openAPI describes the registered routes (generated from the router).
func (s *Server) openAPI(r *gin.Engine) map[string]any {
	paths := map[string]map[string]any{}
	base := s.cfg.APIBase()
	routes := r.Routes()
	sort.Slice(routes, func(i, j int) bool { return routes[i].Path < routes[j].Path })
	for _, rt := range routes {
		if !strings.HasPrefix(rt.Path, base) {
			continue
		}
		p := strings.TrimPrefix(rt.Path, base)
		var params []map[string]any
		parts := strings.Split(p, "/")
		for i, seg := range parts {
			if strings.HasPrefix(seg, ":") {
				params = append(params, map[string]any{"name": seg[1:], "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
				parts[i] = "{" + seg[1:] + "}"
			}
		}
		p = strings.Join(parts, "/")
		if paths[p] == nil {
			paths[p] = map[string]any{}
		}
		op := map[string]any{"operationId": strings.ToLower(rt.Method) + strings.ReplaceAll(strings.ReplaceAll(p, "/", "_"), "{", ""),
			"responses": map[string]any{"200": map[string]string{"description": "OK"}}}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if p != "/auth/login" && p != "/info" && !strings.HasPrefix(p, "/auth/github/") && !strings.HasPrefix(p, "/auth/tg") || p == "/auth/github/bind-url" || p == "/auth/github/unbind" {
			op["security"] = []map[string][]string{{"bearer": {}}}
		}
		paths[p][strings.ToLower(rt.Method)] = op
	}
	return map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]string{"title": "Polaris API", "version": Version},
		"servers": []map[string]string{{"url": base}},
		"paths":   paths,
		"components": map[string]any{"securitySchemes": map[string]any{
			"bearer": map[string]string{"type": "http", "scheme": "bearer", "bearerFormat": "JWT"}}},
	}
}

// WebFS adapts an fs.FS (the embedded build) for the server.
func WebFS(f fs.FS) http.FileSystem { return http.FS(f) }
