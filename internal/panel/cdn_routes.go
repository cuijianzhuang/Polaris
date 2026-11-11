package panel

import "github.com/gin-gonic/gin"

// registerCDNRoutes registers the CDN profile, address-check and probe
// endpoints. It is called from Router in the same position these routes
// used to be registered inline.
func (s *Server) registerCDNRoutes(p *gin.RouterGroup) {
	p.GET("/cdn-profiles/:id/address-tools", s.cdnAddressTools)
	p.POST("/cdn-profiles/:id/check-address", s.checkCDNAddress)
	p.DELETE("/cdn-profiles/:id/check-address", s.resetCDNAddressCheck)
	p.PUT("/cdn-profiles/:id/address-policy", s.updateCDNAddressPolicy)
	p.PUT("/cdn-profiles/:id/check-schedule", s.updateCDNCheckSchedule)
	p.GET("/cdn-profiles/:id/tunnel-config", s.cdnTunnelConfig)
	p.GET("/cdn-profiles/:id/tunnel-agent", s.getTunnelAgent)
	p.PUT("/cdn-profiles/:id/tunnel-agent", s.updateTunnelAgent)
	p.GET("/cdn-regions", s.listCDNRegions)
	p.GET("/cdn-profiles", s.listCDN)
	p.POST("/cdn-profiles", s.createCDN)
	p.PUT("/cdn-profiles/:id", s.updateCDN)
	p.PUT("/cdn-profiles/:id/pinned-hosts", s.updateCDNPinnedHosts)
	p.DELETE("/cdn-profiles/:id", s.deleteCDN)
	p.POST("/cdn-profiles/:id/sync-dns", s.syncCDNDNS)
	p.POST("/cdn-profiles/:id/refresh-ips", s.refreshCDNIPs)
	p.POST("/cdn-profiles/:id/preferred-ips", s.postCDNIPReport)
	p.GET("/cdn-profiles/:id/ip-candidates", s.cdnIPCandidates)
	p.GET("/cdn-shodan", s.getCDNShodan)
	p.PUT("/cdn-shodan", s.putCDNShodan)
	p.POST("/cdn-shodan/refresh", s.refreshCDNShodanNow)
	p.DELETE("/cdn-profiles/:id/ip-reports/:source", s.deleteCDNIPReport)
	p.POST("/cdn-profiles/:id/probes", s.requireSudo, s.pairCDNProbe)
}
