package panel

import (
	_ "embed"
	"html/template"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed assets/portal-ai.html
var portalAIHTML string
var portalAITpl = template.Must(template.New("ai-support").Parse(portalAIHTML))

func (s *Server) aiStatus(c *gin.Context) {
	if !aiOwner(c) {
		return
	}
	cfg, err := s.aiConfig()
	if err != nil {
		serverError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"enabled": cfg.Enabled})
}
func (s *Server) aiPortalPage(c *gin.Context) {
	u := s.portalUser(c)
	if u == nil {
		c.Redirect(http.StatusFound, portalPath+"/login")
		return
	}
	if !s.aiPortalOn() {
		c.Redirect(http.StatusFound, portalPath+"#help")
		return
	}
	s.renderPortalSection(c, http.StatusOK, "assistant", "AI 客服", portalAITpl, gin.H{"Tickets": s.ticketsOn()}, u)
}
