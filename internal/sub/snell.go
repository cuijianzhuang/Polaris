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
