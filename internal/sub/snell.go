package sub

import (
	"regexp"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

var singboxClientVersion = regexp.MustCompile(`(?i)(?:sing-box|singbox|sfa|sfi|sfm|sft)[/\s]+v?(\d+\.\d+\.\d+(?:[-.][a-zA-Z0-9.-]+)?)`)

// An explicit singbox format without a version is a manual export for 1.14+.
// Known old clients must not receive unknown Snell outbounds. Other renderers
// have no verified multi-user encoding and omit Snell entirely.
func FilterSnellClient(nodes []Node, ua string) []Node {
	m := singboxClientVersion.FindStringSubmatch(ua)
	compatible := len(m) >= 2 && core.VersionAtLeast(strings.ToLower(m[1]), model.SnellMinVersion) && !strings.Contains(m[1], "-")
	manual := len(m) < 2 && !containsAny(strings.ToLower(ua), "sing-box", "singbox", "sfa", "sfi", "sfm", "sft", "hiddify")
	if compatible || manual {
		return nodes
	}
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Protocol != model.ProtoSnell {
			out = append(out, n)
		}
	}
	return out
}

var surgeSnellVersion = regexp.MustCompile(`(?i)surge(?:[ /-]*(ios|mac))?[/\s]+(\d+\.\d+(?:\.\d+)?)`)

// v4 is the conservative standard wire format. Known old Surge clients must
// not receive a v6 policy; manual exports require the documented client version.
func FilterSurgeSnell(nodes []Node, ua string) []Node {
	m := surgeSnellVersion.FindStringSubmatch(ua)
	// Some Surge releases send only an internal build number in the UA.
	// An unidentifiable app/platform cannot be declared too old: the admin
	// explicitly selected v6, so retain it with documented minimum versions.
	allowed := true
	if len(m) > 2 {
		lower := strings.ToLower(ua)
		minimum := ""
		if strings.EqualFold(m[1], "ios") || strings.Contains(lower, "iphone") || strings.Contains(lower, "ipad") || strings.Contains(lower, " ios") {
			minimum = "5.20.0"
		} else if strings.EqualFold(m[1], "mac") || strings.Contains(lower, "macintosh") {
			minimum = "6.7.0"
		}
		if minimum != "" {
			allowed = core.VersionAtLeast(m[2], minimum)
		}
	}
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Protocol == model.ProtoSnell && (!n.SnellSinglePSK || (n.SnellVersion == 6 && !allowed)) {
			continue
		}
		out = append(out, n)
	}
	return out
}
