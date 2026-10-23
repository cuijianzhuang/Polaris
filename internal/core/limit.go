package core

import (
	"net"
	"strconv"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// LimitPlan is what a core renderer needs to send limited users through
// the agent's limiter: the users (sorted), and where the limiter and the
// re-entry inbound listen.
type LimitBinding struct{ User, WireUser, Inbound string }

type LimitPlan struct {
	ReentryProtocol string
	Bindings        []LimitBinding
	Users           []string
	LimiterHost     string
	LimiterPort     int
	ReentryHost     string
	ReentryPort     int
	Secret          string
}

// PlanLimits returns the limiter plan for the users of the given inbounds,
// or nil when nothing on this core is limited (or no limiter is wired).
func PlanLimits(w *model.LimitWire, spec *model.NodeSpec, ins []model.Inbound) *LimitPlan {
	if w == nil || len(spec.UserLimits) == 0 {
		return nil
	}
	var all []model.User
	for _, in := range ins {
		all = append(all, in.Users...)
	}
	users := spec.LimitedUsers(all)
	if len(users) == 0 {
		return nil
	}
	lh, lp := splitHostPort(w.Limiter)
	rh, rp := splitHostPort(w.Reentry)
	if lp == 0 || rp == 0 {
		return nil
	}
	var bindings []LimitBinding
	for _, in := range ins {
		if !in.OutboundIPStrategy.IsAuto() {
			for _, u := range spec.LimitedUsers(in.Users) {
				bindings = append(bindings, LimitBinding{User: u, WireUser: model.ScopedLimitUser(u, in.Tag), Inbound: in.Tag})
			}
		}
	}
	for _, u := range users {
		bindings = append(bindings, LimitBinding{User: u, WireUser: u})
	}
	return &LimitPlan{ReentryProtocol: w.ReentryProtocol, Bindings: bindings, Users: users, LimiterHost: lh, LimiterPort: lp, ReentryHost: rh, ReentryPort: rp, Secret: w.Secret}
}

func splitHostPort(a string) (string, int) {
	h, p, err := net.SplitHostPort(a)
	if err != nil {
		return "", 0
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

// AliasRoutes returns routes whose user lists also name the limiter alias
// of each limited user.
func AliasRoutes(spec *model.NodeSpec) []model.RouteRule {
	if len(spec.UserLimits) == 0 {
		return spec.Routes
	}
	out := make([]model.RouteRule, len(spec.Routes))
	for i, r := range spec.Routes {
		r.Match.Users = spec.WithLimitAliases(r.Match.Users)
		out[i] = r
	}
	return out
}
