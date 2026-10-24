package singbox

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// A dedicated PSK authenticates exactly one known owner, but the native Snell
// implementation leaves metadata.User empty in this mode. Keep identity-based
// routing and charging explicit rather than silently treating it as anonymous.
func snellOwnerRoutes(spec *model.NodeSpec) *model.NodeSpec {
	var owners []model.Inbound
	for _, in := range spec.InboundsFor(model.CoreSingbox) {
		if in.SnellDedicated() && len(in.Users) == 1 {
			owners = append(owners, in)
		}
	}
	if len(owners) == 0 {
		return spec
	}
	out := spec.Clone()
	out.Routes = nil
	for _, r := range spec.Routes {
		out.Routes = append(out.Routes, r)
		if len(r.Match.Users) == 0 {
			continue
		}
		for _, in := range owners {
			if !slices.Contains(r.Match.Users, in.Protocol.SnellUserID) || (len(r.Match.Inbound) > 0 && !slices.Contains(r.Match.Inbound, in.Tag)) {
				continue
			}
			clone := r
			clone.Match.Inbound = []string{in.Tag}
			clone.Match.Users = nil
			clone.Match.UserGroups = nil
			out.Routes = append(out.Routes, clone)
		}
	}
	return out
}

func snellAutoInbound(ins []model.Inbound, tag string) bool {
	for _, in := range ins {
		if in.Tag == tag {
			return in.OutboundIPStrategy.IsAuto()
		}
	}
	return false
}

func snellOwnerTraffic(counts map[string][2]int64, owners map[string]string) map[string][2]int64 {
	// Retain tag totals for inbound charts; add each dedicated payload exactly
	// once to its owner. Native user counters are absent on these listeners.
	for tag, owner := range owners {
		v := counts["inbound>>>"+tag]
		cur := counts[owner]
		cur[0] += v[0]
		cur[1] += v[1]
		counts[owner] = cur
	}
	return counts
}

// A failed Render/Validate may leave a pending descriptor that was never
// written. A manual restart must use the actual installed config, not that
// rejected descriptor. Unknown single-PSK files fail closed.
func (a *Adapter) activateSnellOwners() error {
	data, readErr := os.ReadFile(a.configPath())
	digest := sha256.Sum256(data)
	a.mu.Lock()
	owners := map[string]string{}
	switch {
	case readErr == nil && digest == a.pendingSnellDigest:
		owners = a.pendingSnellOwners
	case readErr == nil && digest == a.activeSnellDigest:
		owners = a.snellOwners
	default:
		if (readErr != nil && (len(a.snellOwners) > 0 || len(a.pendingSnellOwners) > 0)) || (readErr == nil && nativeSnellPSK(data)) {
			a.mu.Unlock()
			return fmt.Errorf("Snell ownership does not match the installed configuration")
		}
	}
	a.snellOwners = owners
	a.activeSnellDigest = digest
	a.mu.Unlock()
	a.online.mu.Lock()
	a.online.singleOwners = owners
	a.online.pending = map[string]string{}
	a.online.seen = map[string]map[string]time.Time{}
	a.online.mu.Unlock()
	return nil
}

func nativeSnellPSK(data []byte) bool {
	var cfg struct {
		Inbounds []struct {
			Type  string            `json:"type"`
			Users []json.RawMessage `json:"users"`
		} `json:"inbounds"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return true
	}
	for _, in := range cfg.Inbounds {
		if in.Type == "snell" && len(in.Users) == 0 {
			return true
		}
	}
	return false
}
