package panel

import (
	"bytes"
	"context"
	"errors"
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
	"time"

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
	case errors.As(err, new(commandInputError)):
		badRequest(c, err)
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
	r.Use(s.subHostGate(s.decoy()))

	// Public routes.
	r.GET("/favicon.ico", s.siteIcon)
	r.GET("/apple-touch-icon.png", s.siteIcon)
	r.GET("/sub/:token", s.subscription)
	r.GET(epayNotifyPath, s.epayNotify)
	r.POST(epayNotifyPath, s.epayNotify)
	r.GET(epayReturnPath, s.payReturn)
	r.GET("/sub/p/:token", s.profileSubscription)
	r.GET("/sub/cb/:token", s.subCallbackHandler)
	r.GET("/sub/scheme/:name", s.serveSchemeINI)
	r.GET("/sub/rs/:sig/:enc", s.serveRuleSet)
	r.GET("/install.sh", s.installScript)
	r.GET("/j/:token", s.joinScript)
	r.GET("/download/:file", s.download)

	admin := strings.TrimRight(s.cfg.AdminPath, "/")
	decoy := s.decoy()
	if admin != statusPath { // public status page (off by default)
		r.GET(statusPath, s.statusGate(decoy, s.statusPageHTML))
		r.GET(statusPath+"/data", s.statusGate(decoy, s.statusData))
	}
	if admin != portalPath { // user center (off by default: the decoy answers)
		r.GET(portalPath, s.portalGate(decoy, s.portalPage))
		r.POST(portalPath+"/login", s.portalGate(decoy, s.portalLoginStart))
		r.POST(portalPath+"/poll", s.portalGate(decoy, s.portalLoginPoll))
		r.POST(portalPath+"/logout", s.portalGate(decoy, s.portalLogout))
		r.POST(portalPath+"/reset", s.portalGate(decoy, s.portalReset))
		r.POST(portalPath+"/rename", s.portalGate(decoy, s.portalRename))
		r.POST(portalPath+"/email/code", s.portalGate(decoy, s.portalMailCode))
		r.GET(portalPath+"/forwards", s.portalGate(decoy, s.portalForwards))
		r.POST(portalPath+"/forwards", s.portalGate(decoy, s.portalForwardCreate))
		r.POST(portalPath+"/forwards/:id/delete", s.portalGate(decoy, s.portalForwardDelete))
		r.GET(portalPath+"/tickets", s.portalGate(decoy, s.portalTickets))
		r.POST(portalPath+"/tickets", s.portalGate(decoy, s.portalTicketNew))
		r.GET(portalPath+"/tickets/:no", s.portalGate(decoy, s.portalTicket))
		r.POST(portalPath+"/tickets/:no/reply", s.portalGate(decoy, s.portalTicketAction(false)))
		r.POST(portalPath+"/tickets/:no/close", s.portalGate(decoy, s.portalTicketAction(true)))
		r.POST(portalPath+"/email/verify", s.portalGate(decoy, s.portalMailVerify))
		r.GET(shopWebPath, s.portalGate(decoy, s.webShopPage))
		r.POST(shopWebPath+"/order", s.portalGate(decoy, s.webShopBuy))
		r.POST(shopWebPath+"/redeem", s.portalGate(decoy, s.webShopRedeem))
		r.GET(shopWebPath+"/coupon", s.portalGate(decoy, s.webShopCoupon))
		r.GET(shopWebPath+"/order/:id", s.portalGate(decoy, s.webShopOrderPage))
		r.GET(shopWebPath+"/order/:id/status", s.portalGate(decoy, s.webShopOrderStatus))
		r.POST(shopWebPath+"/order/:id/cancel", s.portalGate(decoy, s.webShopCancel))
	}
	api := r.Group(s.cfg.APIBase())
	api.POST("/auth/login", s.loginGuard, s.login)
	api.POST("/auth/passkeys/login/begin", s.loginGuard, s.beginPasskeyLogin)
	api.POST("/auth/passkeys/login/finish", s.loginGuard, s.finishPasskeyLogin)
	api.POST("/auth/account-recovery", s.loginGuard, s.recoverPasskeyAccount)
	api.POST("/auth/github/login-url", s.loginGuard, s.githubLoginURL)
	api.GET("/auth/github/callback", s.githubCallback)
	api.POST("/auth/github/totp", s.loginGuard, s.githubTOTP)
	api.GET("/info", s.publicInfo)
	api.GET("/brand/logo", s.getBrandLogo)
	api.POST("/auth/tg", s.loginGuard, s.tgLogin)
	api.POST("/auth/tg/start", s.loginGuard, s.tgWebLoginStart)
	api.POST("/auth/tg/poll", s.loginGuard, s.tgWebLoginPoll)
	api.POST("/auth/sso/:id/login-url", s.loginGuard, s.ssoLoginURL)
	api.GET("/auth/sso/callback", s.loginGuard, s.ssoCallback)

	p := api.Group("", s.authRequired, s.scopeGuard, s.licenseManagementGuard, s.trackChanges, s.idempotency)
	p.GET("/ws", func(c *gin.Context) {
		s.handleWS(c.Writer, c.Request, s.sessionCheck(c), nodeScope(c))
	})
	p.GET("/openapi.json", func(c *gin.Context) { c.JSON(http.StatusOK, s.openAPIFor(r, apiTokenOf(c))) })
	p.GET("/auth/me", s.me)
	p.POST("/auth/password", s.changePassword)
	p.GET("/auth/passkeys", s.listPasskeys)
	p.POST("/auth/passkeys/register/begin", s.requireSudo, s.beginPasskeyRegistration)
	p.POST("/auth/passkeys/register/finish", s.requireSudo, s.finishPasskeyRegistration)
	p.DELETE("/auth/passkeys/:pid", s.requireSudo, s.revokePasskey)
	p.POST("/auth/account-recovery-codes", s.requireSudo, s.regenerateAccountRecovery)
	p.POST("/auth/totp/setup", s.requireSudo, s.totpSetup)
	p.POST("/auth/totp/enable", s.totpEnable)
	p.POST("/auth/totp/recovery-codes", s.requireSudo, s.regenRecoveryCodes)
	p.GET("/auth/github", s.githubInfo)
	p.POST("/auth/github/bind-url", s.requireSudo, s.githubBindURL)
	p.POST("/auth/github/unbind", s.requireSudo, s.githubUnbind)
	p.POST("/auth/logout", s.logout)
	p.POST("/auth/sudo", s.sudo)
	p.GET("/auth/sessions", s.listSessions)
	p.DELETE("/auth/sessions/:id", s.revokeSession)
	p.POST("/auth/sessions/revoke-others", s.revokeOtherSessions)
	p.GET("/security", s.securityInfo)
	p.GET("/sso/providers", s.listSSOProviders)
	p.POST("/sso/providers", s.requireSudo, s.createSSOProvider)
	p.PUT("/sso/providers/:id", s.requireSudo, s.updateSSOProvider)
	p.DELETE("/sso/providers/:id", s.requireSudo, s.deleteSSOProvider)
	p.GET("/auth/sso/bindings", s.mySSOBindings)
	p.POST("/auth/sso/:id/bind-url", s.requireSudo, s.ssoBindURL)
	p.DELETE("/auth/sso/bindings/:id", s.requireSudo, s.ssoUnbind)
	p.DELETE("/security/bans/:ip", s.requireSudo, s.unbanIP)

	p.GET("/nodes", s.listNodes)
	p.POST("/nodes", s.createNode)
	p.GET("/nodes/:id", s.getNode)
	p.PUT("/nodes/:id", s.updateNode)
	p.PUT("/nodes/:id/net", s.putNodeNet)
	p.GET("/notifications/policy", s.getNotificationPolicy)
	p.PUT("/notifications/policy", s.putNotificationPolicy)
	p.GET("/probe", s.probePage)
	p.GET("/probe/targets", s.listProbeTargets)
	p.POST("/probe/targets", s.createProbeTarget)
	p.POST("/probe/targets/test", s.testProbeTarget)
	p.PUT("/probe/targets/:id", s.updateProbeTarget)
	p.DELETE("/probe/targets/:id", s.deleteProbeTarget)
	p.PUT("/probe/nodes/:id", s.setNodeStatusHidden)
	p.GET("/nodes/:id/latency", s.nodeLatency)
	p.GET("/tools/route-geo", s.getRouteGeo)
	p.PUT("/tools/route-geo", s.putRouteGeo)
	p.PUT("/tools/route-geo/database", s.uploadRouteGeo)
	p.POST("/tools/route-geo/database/fetch", s.fetchRouteGeo)
	p.POST("/tools/route-geo/lookup", s.lookupRouteGeo)
	p.GET("/tools/route-asn", s.getRouteASN)
	p.POST("/tools/route-asn/lookup", s.lookupRouteASN)
	p.PUT("/tools/route-asn", s.putRouteASN)
	p.PUT("/tools/route-asn/database", s.uploadRouteASN)
	p.POST("/tools/route-asn/database/fetch", s.fetchRouteASN)
	p.GET("/nodes/:id/route-schedule", s.getNodeRouteSchedule)
	p.PUT("/nodes/:id/route-schedule", s.putNodeRouteSchedule)
	p.GET("/nodes/:id/route-tool", s.getNodeRouteTool)
	p.POST("/nodes/:id/route-tool/install", s.requireSudo, s.installNodeRouteTool)
	p.POST("/nodes/:id/route-trace", s.traceNodeRoute)
	p.GET("/nodes/:id/route-traces", s.nodeRouteHistory)
	p.GET("/nodes/:id/route-traces/:traceId", s.nodeRouteDetail)
	p.GET("/nodes/:id/probe-policy", s.getNodeProbePolicy)
	p.PUT("/nodes/:id/probe-policy", s.putNodeProbePolicy)
	p.GET("/nodes/:id/provider", s.getNodeProvider)
	p.PUT("/nodes/:id/provider", s.requireSudo, s.putNodeProvider)
	p.POST("/nodes/:id/provider/refresh", s.refreshNodeProvider)
	p.DELETE("/nodes/:id/provider", s.requireSudo, s.deleteNodeProvider)
	p.POST("/nodes/:id/net-reset", s.resetNodeNet)
	p.GET("/nodes/:id/net-daily", s.nodeNetDaily)
	p.GET("/nodes/:id/limited", s.nodeLimited)
	p.PUT("/nodes/:id/cost", s.putNodeCost)
	p.POST("/nodes/:id/renew", s.renewNodeCost)
	p.GET("/costs", s.listCosts)
	p.PUT("/costs/settings", s.putCostSettings)
	p.DELETE("/nodes/:id", s.deleteNode)
	p.POST("/nodes/:id/install-token", s.installToken)
	p.POST("/nodes/:id/sync", s.syncNode)
	p.POST("/nodes/:id/geodata-update", s.geodataUpdate)
	p.POST("/tools/reality-scan", s.realityScan)
	p.GET("/tools/reality-candidates", s.realityCandidates)
	p.POST("/tools/reality-discover", s.realityDiscover)
	p.GET("/tools/shodan", s.shodanInfo)
	p.POST("/tools/vlessenc", s.genVLESSEnc)
	p.POST("/tools/mldsa65", s.genMLDSA65)
	p.POST("/nodes/:id/upgrade-agent", s.upgradeAgentAPI)
	p.GET("/nodes/:id/users", s.nodeUsersAPI)
	p.POST("/nodes/:id/users", s.bindNodeUsers)
	p.POST("/nodes/upgrade-agents", s.upgradeAllAgents)
	p.GET("/rollouts", s.listRollouts)
	p.POST("/rollouts", s.createRollout)
	p.GET("/rollouts/:id", s.getRollout)
	p.POST("/rollouts/:id/:action", s.rolloutAction)
	p.POST("/nodes/:id/rollback", s.rollbackNode)
	p.POST("/nodes/:id/revoke", s.revokeNode)
	p.GET("/nodes/:id/revisions", s.listRevisions)
	p.GET("/nodes/:id/spec", s.nodeSpec)
	p.GET("/nodes/:id/render", s.renderNodeCore)
	p.GET("/nodes/:id/cores", s.listCores)
	p.GET("/nodes/:id/snell-readiness", s.getSnellReadiness)
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
	p.PUT("/inbounds/:id/publication", s.setInboundPublication)
	p.PUT("/inbounds/:id/relay-access", s.setRelayAccess)
	p.PUT("/inbounds/:id/outbound-ip", s.setOutboundIP)
	p.GET("/inbounds/:id/relays", s.listInboundRelays)
	p.POST("/inbounds/:id/relays/preview", s.previewInboundRelay)
	p.POST("/inbounds/:id/relays/batch/preview", s.previewInboundRelayBatch)
	p.POST("/inbounds/:id/relays/batch", s.createInboundRelayBatch)
	p.POST("/inbounds/:id/relays", s.createInboundRelay)
	p.POST("/inbounds/:id/relays/:rid/change-preview", s.previewRelayChange)
	p.PUT("/inbounds/:id/relays/:rid/state", s.setInboundRelayState)
	p.PUT("/inbounds/:id/relays/:rid/metadata", s.updateInboundRelayMetadata)
	p.POST("/inbounds/:id/relay-batches/:bid/change-preview", s.previewRelayChange)
	p.GET("/inbounds/:id/relay-batches/:bid", s.getInboundRelayBatch)
	p.PUT("/inbounds/:id/relay-batches/:bid/state", s.setInboundRelayBatchState)
	p.DELETE("/inbounds/:id/relay-batches/:bid", s.deleteInboundRelayBatch)
	p.DELETE("/inbounds/:id/relays/:rid", s.deleteInboundRelay)
	p.POST("/inbounds/:id/relays/:rid/diagnose", s.diagnoseInboundRelay)
	p.POST("/inbounds/:id/relays/:rid/diagnose/business", s.diagnoseInboundRelayBusiness)
	p.POST("/inbounds/:id/probe", s.probeInboundAPI)
	p.POST("/inbounds/bulk", s.bulkInbounds)
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
	p.POST("/users/:id/renew", s.renewUser)
	p.POST("/users/:id/add-traffic", s.addUserTraffic)
	p.POST("/users/:id/plan", s.setUserPlan)
	p.POST("/users/:id/reset-token", s.resetUserToken)
	p.GET("/users/:id/node-quotas", s.listNodeQuotas)
	p.PUT("/users/:id/node-quotas/:node", s.putNodeQuota)
	p.DELETE("/users/:id/node-quotas/:node", s.deleteNodeQuota)
	p.GET("/users/:id/traffic-sources", s.userTrafficSources)
	p.GET("/users/:id/links", s.userLinks)

	p.GET("/groups", s.listGroups)
	p.POST("/groups", s.createGroup)
	p.PUT("/groups/:id", s.updateGroup)
	p.DELETE("/groups/:id", s.deleteGroup)
	p.GET("/plans", s.listPlans)
	p.POST("/plans", s.createPlan)
	p.PUT("/plans/:id", s.updatePlan)
	p.DELETE("/plans/:id", s.deleteGroup)
	p.GET("/traffic-packs", s.listPacks)
	p.POST("/traffic-packs", s.createPack)
	p.PUT("/traffic-packs/:id", s.updatePack)
	p.DELETE("/traffic-packs/:id", s.deletePack)
	p.POST("/users/:id/pack", s.addUserPack)
	p.GET("/orders", s.listOrders)
	p.POST("/orders/:id/fulfil", s.requireSudo, s.fulfilOrder)
	p.POST("/orders/:id/refund", s.requireSudo, s.refundOrder)
	p.POST("/orders/:id/cancel", s.adminCancelOrder)
	p.DELETE("/orders", s.deleteOrders)
	p.GET("/tickets", s.listTickets)
	p.GET("/tickets/:id", s.getTicket)
	p.POST("/tickets/:id/reply", s.replyTicket)
	p.POST("/tickets/:id/close", s.setTicketStatus(true))
	p.POST("/tickets/:id/reopen", s.setTicketStatus(false))
	p.DELETE("/tickets/:id", s.deleteTicket)
	p.GET("/coupons", s.listCoupons)
	p.POST("/coupons", s.createCoupons)
	p.PUT("/coupons/:code", s.updateCoupon)
	p.DELETE("/coupons/:code", s.deleteCoupon)
	p.GET("/coupons/:code/uses", s.couponUses)
	p.GET("/redeem-codes", s.listCodes)
	p.POST("/redeem-codes", s.createCodes)
	p.DELETE("/redeem-codes", s.deleteCodes)

	p.GET("/stats/overview", s.statsOverview)
	p.GET("/stats/operational", s.statsOperational)
	p.GET("/stats/traffic", s.statsTraffic)
	p.GET("/stats/tags", s.tagStats)
	p.GET("/stats/nodes", s.nodeTrafficAPI)
	p.GET("/stats/report", s.statsReport)
	p.GET("/stats/export", s.statsExport)
	p.GET("/nodes/:id/metrics", s.nodeMetrics)
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
	p.POST("/chains/:id/change-preview", s.previewRelayChange)
	p.DELETE("/chains/:id", s.deleteChain)
	p.POST("/chains/:id/probe", s.probeChain)
	p.GET("/nodes/:id/outbounds", s.listOutbounds)
	p.POST("/nodes/:id/outbounds", s.createOutbound)
	p.POST("/nodes/:id/warp", s.addWARP)
	p.PUT("/nodes/:id/outbounds/:oid", s.updateOutbound)
	p.DELETE("/nodes/:id/outbounds/:oid", s.deleteOutbound)
	p.GET("/routing/presets", s.listRulePresets)
	p.GET("/routing/geo", s.geoCategories)
	p.GET("/routing/geo/geosite/:name", s.geoEntries)
	p.POST("/routing/presets/apply", s.applyRulePreset)
	p.GET("/nodes/:id/routing", s.getRouting)
	p.PUT("/nodes/:id/routing", s.putRouting)
	p.GET("/dns", s.getDNS)
	p.PUT("/dns/:scope", s.putDNS)
	p.GET("/rulesets", s.listRuleSets)
	p.POST("/rulesets", s.createRuleSet)
	p.PUT("/rulesets/:id", s.updateRuleSet)
	p.DELETE("/rulesets/:id", s.deleteRuleSet)
	p.POST("/rulesets/:id/refresh", s.refreshRuleSetHandler)

	p.GET("/dns-providers", s.listDNSProviders)
	p.POST("/dns-providers", s.saveDNSProvider)
	p.PUT("/dns-providers/:id", s.saveDNSProvider)
	p.DELETE("/dns-providers/:id", s.deleteDNSProvider)
	p.GET("/domains", s.listBindings)
	p.POST("/domains", s.saveBinding)
	p.PUT("/domains/:id", s.saveBinding)
	p.POST("/domains/auto-bind", s.autoBindAll)
	p.GET("/system/update", s.getPanelUpdate)
	p.GET("/cluster", s.getCluster)
	p.GET("/operational-health", s.getOperationalHealth)
	p.GET("/ready", s.readiness)
	p.POST("/orders/:id/retry", s.requireSudo, s.retryOrderPaymentHandler)
	p.POST("/system/update", s.requireSudo, s.postPanelUpdate)
	p.GET("/system/access", s.getAccess)
	p.POST("/system/access", s.requireSudo, s.postAccess)
	p.GET("/license", s.getLicense)
	p.POST("/license", s.requireSudo, s.postLicense)
	p.POST("/license/activate", s.requireSudo, s.activateLicenseAPI)
	p.POST("/license/deactivate", s.requireSudo, s.deactivateLicenseAPI)
	p.POST("/license/server", s.setLicenseServer)
	p.POST("/license/check", s.checkLicenseAPI)
	p.POST("/domains/sync-all", s.syncAllBindingsAPI)
	p.POST("/domains/:id/sync", s.syncBindingAPI)
	p.DELETE("/domains/:id", s.deleteBinding)

	p.GET("/templates", s.listTemplates)
	p.POST("/templates", s.createTemplate)
	p.PUT("/templates/:id", s.updateTemplate)
	p.DELETE("/templates/:id", s.deleteTemplate)
	p.POST("/templates/:id/preview", s.previewTemplate)
	p.POST("/templates/:id/apply", s.applyTemplate)
	p.GET("/templates/:id/runs/:run", s.getTemplateRun)
	p.GET("/templates/:id/runs", s.listTemplateRuns)
	p.POST("/templates/:id/runs/:run/retry", s.retryTemplateRun)
	p.POST("/templates/:id/runs/:run/reality-check", s.checkTemplateReality)
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
	p.GET("/sub-sources/:id/refreshes", s.sourceRefreshesAPI)
	p.GET("/sub-source-nodes", s.sourceResourcesAPI)
	p.POST("/sub-source-nodes/:nid/references", s.bindSourceNodeAPI)
	p.DELETE("/sub-source-nodes/:nid", s.deleteSourceNodeAPI)
	p.GET("/sub-processing-groups", s.listProcessingGroups)
	p.POST("/sub-processing-groups", s.createProcessingGroup)
	p.GET("/sub-processing-groups/:id/versions", s.processingGroupVersions)
	p.GET("/sub-processing-groups/:id/versions/:revision", s.processingGroupVersion)
	p.POST("/sub-processing-groups/:id/preview", s.previewProcessingGroup)
	p.PUT("/sub-processing-groups/:id", s.updateProcessingGroup)
	p.DELETE("/sub-processing-groups/:id", s.deleteProcessingGroup)
	p.GET("/sub-profiles", s.listProfiles)
	p.POST("/sub-profiles", s.saveProfile)
	p.POST("/sub-profiles/preview", s.previewProfile)
	p.PUT("/sub-profiles/:id", s.saveProfile)
	p.DELETE("/sub-profiles/:id", s.deleteProfile)
	p.POST("/sub-profiles/:id/reset-token", s.resetProfileToken)
	p.GET("/telegram", s.tgStatus)
	p.GET("/telegram/admins", s.listTGAdmins)
	p.POST("/telegram/bind-code", s.requireSudo, s.tgBindCode)
	p.PUT("/telegram/admins/:id", s.requireSudo, s.updateTGAdmin)
	p.DELETE("/telegram/admins/:id", s.requireSudo, s.deleteTGAdmin)
	p.POST("/telegram/test", s.tgTest)
	p.POST("/telegram/broadcast", s.tgBroadcast)
	p.POST("/telegram/template-preview", s.previewReminder)
	p.GET("/backup", s.backupInfo)
	p.POST("/backup", s.backupCreate)
	p.POST("/backup/restore", s.requireSudo, s.backupRestore)
	p.GET("/backup/files/:name", s.requireSudo, s.backupDownload)
	p.DELETE("/backup/files/:name", s.requireSudo, s.backupDelete)
	p.POST("/backup/files/:name/restore", s.requireSudo, s.backupRestore)
	p.POST("/backup/store/test", s.requireSudo, s.backupStoreTest)
	p.POST("/mail/test", s.requireSudo, s.testMail)
	p.GET("/settings", s.getSettings)
	p.GET("/tokens", s.listTokens)
	p.GET("/plugins", s.listPlugins)
	p.POST("/plugins/preview", s.requireSudo, s.previewPlugin)
	p.POST("/plugins", s.requireSudo, s.installPlugin)
	p.GET("/plugins/:id", s.getPlugin)
	p.PUT("/plugins/:id", s.requireSudo, s.updatePlugin)
	p.PUT("/plugins/:id/settings", s.requireSudo, s.updatePluginSettings)
	p.POST("/plugins/:id/refresh", s.requireSudo, s.refreshPlugin)
	p.DELETE("/plugins/:id", s.requireSudo, s.deletePlugin)
	p.GET("/plugins/:id/pages/:page", s.pluginPageURL)
	p.PUT("/plugin-transform-order", s.requireSudo, s.setTransformOrder)
	p.GET("/plugin/config", s.pluginOwnConfig)
	p.GET("/webhooks", s.listWebhooks)
	p.POST("/webhooks", s.requireSudo, s.createWebhook)
	p.PUT("/webhooks/:id", s.requireSudo, s.updateWebhook)
	p.DELETE("/webhooks/:id", s.deleteWebhook)
	p.POST("/webhooks/:id/test", s.testWebhook)
	p.GET("/webhooks/:id/deliveries", s.listDeliveries)
	p.POST("/webhook-deliveries/:id/retry", s.retryDelivery)
	p.GET("/tokens/self", s.tokenSelf)
	p.POST("/tokens", s.requireSudo, s.createToken)
	p.DELETE("/tokens/:id", s.requireSudo, s.deleteToken)
	p.PUT("/brand/logo", s.putBrandLogo)
	p.DELETE("/brand/logo", s.deleteBrandLogo)
	p.PUT("/settings", s.putSettings)
	p.GET("/audit-logs", s.listAudit)

	// Admin SPA under the admin path; everything else gets the decoy.
	var spa gin.HandlerFunc
	if s.webFS != nil {
		spa = s.spaHandler(admin)
	}
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
	c.JSON(http.StatusOK, gin.H{"title": s.st.GetSetting("site_title", "Polaris"), "logo": s.brandLogoVersion(), "version": Version, "githubLogin": s.githubEnabled(), "telegramLogin": s.tgWebLoginEnabled(),
		"passkeyLogin": s.passkeysAvailable(), "sso": s.publicSSOProviders()})
}

// download serves agent binaries placed in <data_dir>/downloads.
func (s *Server) download(c *gin.Context) {
	name := filepath.Base(c.Param("file"))
	// polaris-agent-linux-<arch>[.sha256]: fetched from the release on first
	// request when this architecture is not cached yet.
	if arch, ok := strings.CutPrefix(strings.TrimSuffix(name, ".sha256"), "polaris-agent-linux-"); ok {
		if !validArch(arch) {
			notFound(c)
			return
		}
		if !s.agentCurrent(arch) {
			if !s.subLimit.Allow(clientIP(c.Request)) {
				c.String(http.StatusTooManyRequests, "too many requests")
				return
			}
			ctx, cancel := context.WithTimeout(c.Request.Context(), 4*time.Minute)
			err := s.ensureAgent(ctx, arch)
			cancel()
			if err != nil {
				c.String(http.StatusNotFound, "agent for %s unavailable: %v", arch, err)
				return
			}
		}
		if strings.HasSuffix(name, ".sha256") {
			c.String(http.StatusOK, s.agentSHA(arch)+"\n")
			return
		}
	} else if name != "install-panel.sh" { // the polaris tool for nodes
		notFound(c)
		return
	} else if _, err := os.Stat(s.toolPath()); err != nil { // fetch it once from the release
		if !s.subLimit.Allow(clientIP(c.Request)) {
			c.String(http.StatusTooManyRequests, "too many requests")
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), time.Minute)
		_ = s.ensureTool(ctx)
		cancel()
	}
	p := filepath.Join(s.cfg.DataDir, "downloads", name)
	if _, err := os.Stat(p); err != nil {
		notFound(c)
		return
	}
	c.FileAttachment(p, name)
}

func (s *Server) openAPIFor(r *gin.Engine, token *store.APIToken) map[string]any {
	paths := map[string]map[string]any{}
	base := s.cfg.APIBase()
	routes := r.Routes()
	sort.Slice(routes, func(i, j int) bool { return routes[i].Path < routes[j].Path })
	for _, rt := range routes {
		if !strings.HasPrefix(rt.Path, base) {
			continue
		}
		p := strings.TrimPrefix(rt.Path, base)
		if token != nil && !tokenAllows(token, rt.Method+" "+p) {
			continue
		}
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
		public := p == "/auth/login" || p == "/info" || p == "/brand/logo" && rt.Method == "GET" || strings.HasPrefix(p, "/auth/github/") || strings.HasPrefix(p, "/auth/tg") ||
			p == "/auth/sso/callback" || strings.HasPrefix(p, "/auth/sso/") && strings.HasSuffix(p, "/login-url")
		if !public || p == "/auth/github/bind-url" || p == "/auth/github/unbind" {
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
			"bearer": map[string]string{"type": "http", "scheme": "bearer", "bearerFormat": "JWT or plt_ API token"}}},
	}
}

// WebFS adapts an fs.FS (the embedded build) for the server.
func WebFS(f fs.FS) http.FileSystem { return http.FS(f) }
