package sub

import (
	"html/template"
	"strings"
	"testing"
)

func TestPortalLayoutEscapesFragmentAndKeepsNavigation(t *testing.T) {
	body := template.Must(template.New("body").Parse(`<section><h1>{{.Text}}</h1></section><script>var value={{.Text}};</script>`))
	attack := `</script><img src=x onerror=alert(1)>`
	page, err := RenderPortalSection(PortalNavigation{Title: attack, Username: attack, Active: "orders", PageTitle: "订单详情", ShopURL: "/u/shop", TicketsURL: "/u/tickets"}, body, map[string]string{"Text": attack})
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	if strings.Contains(html, attack) || !strings.Contains(html, `\u003c/script\u003e`) {
		t.Fatal("fragment escaped data became executable")
	}
	for _, want := range []string{`href="/u#subscription"`, `href="/u/shop"`, `href="/u/tickets"`, `aria-current="page"`, `&lt;/script&gt;`} {
		if !strings.Contains(html, want) {
			t.Fatal("missing navigation/escaping", want)
		}
	}
	page, err = RenderPortalSection(PortalNavigation{Title: "Polaris", Guest: true, Active: "shop", PageTitle: "商店"}, body, map[string]string{"Text": "guest"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), `href="/u#orders"`) || strings.Contains(string(page), `href="/u/tickets"`) || !strings.Contains(string(page), `href="/u/login"`) {
		t.Fatal("guest chrome exposes authenticated destinations")
	}
}
