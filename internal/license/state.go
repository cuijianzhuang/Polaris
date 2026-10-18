package license

import (
	"slices"
	"time"
)

// Grace is how long an online license keeps working while the license
// server cannot be reached; afterwards administrative changes are read-only.
const Grace = 14 * 24 * time.Hour

// Inputs is what the panel knows; Evaluate turns it into the State in effect.
type Inputs struct {
	License    *License  // installed and verified (nil = none)
	Revoked    bool      // the license server said so
	LastCheck  time.Time // last successful online check (online licenses)
	FirstSeen  time.Time // first local installation, only a fallback before any online check
	TrialUntil time.Time // paid features unlocked for upgrading installs
	Released   time.Time // release date of this build (zero = unknown)
	Now        time.Time
	// Unenforced: this build carries no license keys (licensing is not set
	// up yet), so nothing is locked.
	Unenforced bool
}

// State is the license in effect.
type State struct {
	Plan               string     `json:"plan"`   // community / trial / pro / enterprise
	Source             string     `json:"source"` // none / license / trial
	Limits             Limits     `json:"limits"`
	Features           []string   `json:"features"`
	ExpiresAt          string     `json:"expiresAt,omitempty"`
	Updates            string     `json:"updatesUntil,omitempty"`
	TrialUntil         *time.Time `json:"trialUntil,omitempty"`
	Warnings           []string   `json:"warnings"`
	ManagementReadOnly bool       `json:"managementReadOnly"`
}

// Allow reports whether a paid feature is available.
func (s State) Allow(f string) bool { return slices.Contains(s.Features, f) }

// Limit returns a cap (0 = unlimited).
func (s State) Limit(kind string) int {
	switch kind {
	case LimitNodes:
		return s.Limits.Nodes
	case LimitUsers:
		return s.Limits.Users
	case LimitPlugins:
		return s.Limits.Plugins
	}
	return 0
}

// Evaluate decides what is in effect. A license that expired, was revoked
// or does not cover this release falls back to the community edition (the
// trial still applies while it lasts). An unreachable license server keeps
// runtime entitlements, but administrative changes are blocked after Grace.
func Evaluate(in Inputs) State {
	st := State{Plan: "community", Source: "none", Limits: Community, Features: []string{}, Warnings: []string{}}
	if in.Unenforced {
		st.Plan, st.Source, st.Limits = "open", "build", Limits{}
		st.Features = append([]string{}, AllFeatures...)
		return st
	}
	if l := in.License; l != nil {
		switch {
		case in.Revoked:
			st.Warnings = append(st.Warnings, "许可证已被吊销，已恢复为社区版")
		case l.Expired(in.Now):
			st.Warnings = append(st.Warnings, "许可证已于 "+l.ExpiresAt+" 到期，已恢复为社区版；已生效的配置保持不变")
		case !l.CoversRelease(in.Released):
			st.Warnings = append(st.Warnings, "当前版本发布于更新期限（"+l.UpdatesUntil+"）之后，请续期更新服务或回到之前的版本")
		default:
			st.Plan, st.Source = l.Plan, "license"
			st.Limits, st.Features = l.Limits, l.Granted()
			st.ExpiresAt, st.Updates = l.ExpiresAt, l.UpdatesUntil
			if t, ok := day(l.ExpiresAt); ok {
				if left := t.Add(24 * time.Hour).Sub(in.Now); left < 30*24*time.Hour {
					st.Warnings = append(st.Warnings, "许可证将于 "+l.ExpiresAt+" 到期")
				}
			}
			if !l.Offline {
				last := in.LastCheck
				if last.IsZero() {
					last = in.FirstSeen
				}
				if !last.IsZero() && in.Now.Sub(last) > Grace {
					st.ManagementReadOnly = true
					st.Warnings = append(st.Warnings, "超过 14 天未成功校验许可证，管理修改已暂停；已有代理继续运行，请联网校验或导入离线许可证")
				}
			}
			return st
		}
	}
	if !in.TrialUntil.IsZero() && in.Now.Before(in.TrialUntil) {
		t := in.TrialUntil
		st.Plan, st.Source, st.TrialUntil = "trial", "trial", &t
		st.Features = append([]string{}, AllFeatures...)
		if in.TrialUntil.Sub(in.Now) < 7*24*time.Hour {
			st.Warnings = append(st.Warnings, "高级功能试用即将结束")
		}
	}
	return st
}
