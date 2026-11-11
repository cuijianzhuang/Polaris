package panel

import "github.com/gin-gonic/gin"

// registerRouteToolRoutes registers the route-database (ASN/city),
// schedule and trace endpoints. It is called from Router in the same
// position these routes used to be registered inline.
func (s *Server) registerRouteToolRoutes(p *gin.RouterGroup) {
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
}
