// Package flags provides bundled flag artwork for browsers whose system emoji
// font does not render a regional flag. Subscription payloads remain plain text.
package flags

import (
	_ "embed"
	"html/template"
	"strings"
)

//go:embed taiwan.svg
var taiwanSVG string

// Taiwan returns trusted, bundled SVG markup, with no external image request.
func Taiwan() template.HTML { return template.HTML(taiwanSVG) }

// Text escapes untrusted display text before replacing the Taiwan flag emoji.
// Use only in HTML element contents, never in attributes, URLs or scripts.
func Text(text string) template.HTML {
	return template.HTML(strings.ReplaceAll(template.HTMLEscapeString(text), "🇹🇼", taiwanSVG))
}
