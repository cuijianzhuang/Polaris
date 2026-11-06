package sub

import (
	"strings"
	"testing"
)

func TestPageTaiwanFlagKeepsShareLinkAndEscapesName(t *testing.T) {
	name := `🇹🇼 Taipei <img src=x onerror=alert(1)>`
	link := "vless://fixture@example.com:443#%F0%9F%87%B9%F0%9F%87%BC%20Taipei"
	body, err := RenderPage(Info{Title: "Flag fixture", URL: "https://example.com/sub/fixture", Links: []PageLink{{Name: name, Protocol: "VLESS", Link: link}}})
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, `<svg `) || strings.Contains(html, `<img src=x`) || !strings.Contains(html, `&lt;img src=x onerror=alert(1)&gt;`) || !strings.Contains(html, `data-copy="`+link+`"`) {
		t.Fatalf("flag changed unsafe name or share link: %s", html)
	}
	if Flag("tw") != "🇹🇼" {
		t.Fatal("subscription wire-format flag changed")
	}
}
