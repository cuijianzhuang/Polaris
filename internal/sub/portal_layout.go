package sub

import (
	"bytes"
	"html/template"
)

// PortalNavigation is the shared chrome for authenticated service pages and
// guest checkout. Feature links are supplied by the server's effective switches.
type PortalNavigation struct {
	Title, Username, Email, Active, PageTitle string
	ShopURL, TicketsURL                       string
	Guest, Registration                       bool
}

// RenderPortalSection renders an application's static html/template inside the
// user-center layout. Callers supply a parsed, trusted application template;
// its data is escaped before being inserted as HTML. Never pass user templates.
func RenderPortalSection(nav PortalNavigation, body *template.Template, data any) ([]byte, error) {
	var fragment, document bytes.Buffer
	if err := body.Execute(&fragment, data); err != nil {
		return nil, err
	}
	values := map[string]any{
		"Title": nav.Title, "Username": nav.Username, "Email": nav.Email,
		"PortalActive": nav.Active, "PortalPageTitle": nav.PageTitle,
		"PortalShopURL": pageURL(nav.ShopURL), "PortalTicketsURL": pageURL(nav.TicketsURL),
		"PortalGuest": nav.Guest, "PortalRegistration": nav.Registration,
		"Content": template.HTML(fragment.String()), // escaped static application template above
	}
	if err := pageTpl.ExecuteTemplate(&document, "portal-document", values); err != nil {
		return nil, err
	}
	return document.Bytes(), nil
}
