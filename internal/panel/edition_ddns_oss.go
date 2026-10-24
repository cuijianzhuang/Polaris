//go:build !pro

package panel

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Community build: subscription conversion (sources and groups) and DDNS
// (domain bindings, node addresses following their IP) are paid features
// compiled from polaris-pro. The pages load with nothing to show; changes
// answer 402 like a missing license.

func proOnly(f string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusPaymentRequired, gin.H{"error": "这是专业版功能，社区版构建不包含（许可证页面）", "feature": f, "plan": "pro"})
	}
}

func emptyList(c *gin.Context) { c.JSON(http.StatusOK, []any{}) }

// --- subscription conversion ---

func (s *Server) listSources(c *gin.Context)    { emptyList(c) }
func (s *Server) saveSource(c *gin.Context)     { proOnly(license.FeatureSubConvert)(c) }
func (s *Server) deleteSource(c *gin.Context)   { proOnly(license.FeatureSubConvert)(c) }
func (s *Server) refreshSource(c *gin.Context)  { proOnly(license.FeatureSubConvert)(c) }
func (s *Server) sourceNodesAPI(c *gin.Context) { emptyList(c) }
func (s *Server) listProfiles(c *gin.Context)   { emptyList(c) }
func (s *Server) saveProfile(c *gin.Context)    { proOnly(license.FeatureSubConvert)(c) }
func (s *Server) deleteProfile(c *gin.Context)  { proOnly(license.FeatureSubConvert)(c) }
func (s *Server) previewProfile(c *gin.Context) { proOnly(license.FeatureSubConvert)(c) }
func (s *Server) resetProfileToken(c *gin.Context) {
	proOnly(license.FeatureSubConvert)(c)
}

// profileSubscription: group subscriptions are not served.
func (s *Server) profileSubscription(c *gin.Context) {
	c.String(http.StatusNotFound, "subscription conversion is not available in this build")
}

func (s *Server) refreshDueSources() {}

// --- DDNS ---

func (s *Server) listBindings(c *gin.Context)       { emptyList(c) }
func (s *Server) saveBinding(c *gin.Context)        { proOnly(license.FeatureDDNS)(c) }
func (s *Server) deleteBinding(c *gin.Context)      { proOnly(license.FeatureDDNS)(c) }
func (s *Server) syncBindingAPI(c *gin.Context)     { proOnly(license.FeatureDDNS)(c) }
func (s *Server) syncAllBindingsAPI(c *gin.Context) { proOnly(license.FeatureDDNS)(c) }
func (s *Server) autoBindAll(c *gin.Context)        { proOnly(license.FeatureDDNS)(c) }

func (s *Server) syncAllBindings()                                {}
func (s *Server) autoBindAfterEnroll(string)                      {}
func (s *Server) ddnsIPChanged(*store.Node, string, string, bool) {}

// Reusable subscription processing groups follow the existing conversion gate.
func (s *Server) listProcessingGroups(c *gin.Context)    { emptyList(c) }
func (s *Server) processingGroupVersions(c *gin.Context) { proOnly(license.FeatureSubConvert)(c) }
func (s *Server) createProcessingGroup(c *gin.Context)   { proOnly(license.FeatureSubConvert)(c) }
func (s *Server) previewProcessingGroup(c *gin.Context)  { proOnly(license.FeatureSubConvert)(c) }
func (s *Server) updateProcessingGroup(c *gin.Context)   { proOnly(license.FeatureSubConvert)(c) }
func (s *Server) deleteProcessingGroup(c *gin.Context)   { proOnly(license.FeatureSubConvert)(c) }

func (s *Server) processingGroupVersion(c *gin.Context) { proOnly(license.FeatureSubConvert)(c) }
