//go:build !pro

package panel

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/license"
)

// Community build: the custom logo is a paid feature compiled from
// polaris-pro. The panel keeps its own logo; uploads answer 402.

func (s *Server) brandLogoVersion() string    { return "" }
func (s *Server) getBrandLogo(c *gin.Context) { c.Status(http.StatusNotFound) }
func (s *Server) putBrandLogo(c *gin.Context) { proOnly(license.FeatureBranding)(c) }
func (s *Server) deleteBrandLogo(c *gin.Context) {
	s.st.SetSetting(brandLogoKey, "")
	c.Status(http.StatusNoContent)
}
