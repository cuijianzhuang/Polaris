package panel

import (
	"html/template"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"github.com/gin-gonic/gin"
)

func (s *Server) renderPortalSection(c *gin.Context, status int, active, title string, tpl *template.Template, data any, user *store.User) {
	nav := sub.PortalNavigation{Title: s.subTitle(s.st.GetSetting("site_title", "Polaris")), Active: active, PageTitle: title, Guest: user == nil, Registration: s.mailRegisterOn()}
	if user != nil {
		nav.Username, nav.Email = user.Username, user.Email
	}
	if s.webShopOn() {
		nav.ShopURL = shopWebPath
	}
	if s.ticketsOn() {
		nav.TicketsURL = portalPath + "/tickets"
	}
	if user != nil && s.aiPortalOn() {
		nav.AIURL = portalPath + "/assistant"
	}
	page, err := sub.RenderPortalSection(nav, tpl, data)
	if err != nil {
		serverError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Data(status, "text/html; charset=utf-8", page)
}
