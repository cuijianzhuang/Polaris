package xray

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
)

// HotApply moves the running Xray from oldCfg to newCfg through its API when
// the two differ only in routing rules, balancers and outbounds other than the
// default (first) one: rules and balancers are replaced in one call
// (`xray api adrules`, which swaps the whole set atomically), outbounds are
// added and removed by tag. Connections through untouched outbounds survive.
// Any other difference returns core.ErrNotSupported and the agent restarts.
func (a *Adapter) HotApply(ctx context.Context, oldCfg, newCfg []byte) error {
	if a.opt.BinPath == "" {
		return core.ErrNotSupported
	}
	plan, err := planHot(oldCfg, newCfg)
	if err != nil {
		return err
	}
	if len(plan.remove) > 0 {
		if err := a.apiCmd(ctx, "rmo", nil, plan.remove...); err != nil {
			return err
		}
	}
	if len(plan.add) > 0 {
		if err := a.apiCmd(ctx, "ado", map[string]any{"outbounds": plan.add}); err != nil {
			return err
		}
	}
	if plan.routing != nil {
		if err := a.apiCmd(ctx, "adrules", map[string]any{"routing": plan.routing}); err != nil {
			return err
		}
	}
	if len(plan.drop) > 0 {
		// Gone outbounds go last, once no rule points at them any more.
		if err := a.apiCmd(ctx, "rmo", nil, plan.drop...); err != nil {
			return err
		}
	}
	return nil
}

type hotPlan struct {
	remove  []string          // changed outbounds, removed before re-adding
	add     []json.RawMessage // changed and new outbounds, in config order
	drop    []string          // outbounds no longer in the config
	routing json.RawMessage   // new routing section, nil when unchanged
}

// planHot works out the API calls between two rendered configs, or returns
// core.ErrNotSupported when a restart is needed.
func planHot(oldCfg, newCfg []byte) (*hotPlan, error) {
	var o, n map[string]json.RawMessage
	if json.Unmarshal(oldCfg, &o) != nil || json.Unmarshal(newCfg, &n) != nil {
		return nil, core.ErrNotSupported
	}
	if !hasRoutingService(o["api"]) {
		return nil, core.ErrNotSupported
	}
	for k := range union(o, n) {
		if k != "routing" && k != "outbounds" && !jsonEqual(o[k], n[k]) {
			return nil, core.ErrNotSupported
		}
	}
	var or, nr map[string]json.RawMessage
	if json.Unmarshal(o["routing"], &or) != nil || json.Unmarshal(n["routing"], &nr) != nil {
		return nil, core.ErrNotSupported
	}
	for k := range union(or, nr) {
		if k != "rules" && k != "balancers" && !jsonEqual(or[k], nr[k]) {
			return nil, core.ErrNotSupported // domainStrategy is fixed at start
		}
	}
	var oo, no []json.RawMessage
	if json.Unmarshal(o["outbounds"], &oo) != nil || json.Unmarshal(n["outbounds"], &no) != nil ||
		len(oo) == 0 || len(no) == 0 || !jsonEqual(oo[0], no[0]) {
		return nil, core.ErrNotSupported // the first outbound is Xray's default
	}
	oldBy := map[string]json.RawMessage{}
	for _, ob := range oo {
		oldBy[tagOf(ob)] = ob
	}
	p := &hotPlan{}
	seen := map[string]bool{}
	for _, ob := range no {
		tag := tagOf(ob)
		if tag == "" || seen[tag] {
			return nil, core.ErrNotSupported
		}
		seen[tag] = true
		prev, ok := oldBy[tag]
		switch {
		case !ok:
			p.add = append(p.add, ob)
		case !jsonEqual(prev, ob):
			p.remove = append(p.remove, tag)
			p.add = append(p.add, ob)
		}
	}
	for _, ob := range oo {
		if t := tagOf(ob); !seen[t] {
			p.drop = append(p.drop, t)
		}
	}
	if !jsonEqual(o["routing"], n["routing"]) {
		p.routing = n["routing"]
	}
	if len(p.remove) == 0 && len(p.add) == 0 && len(p.drop) == 0 && p.routing == nil {
		return nil, core.ErrNotSupported // nothing the API can do; let the caller decide
	}
	return p, nil
}

func hasRoutingService(api json.RawMessage) bool {
	var v struct{ Services []string }
	return json.Unmarshal(api, &v) == nil && slices.Contains(v.Services, "RoutingService")
}

func tagOf(ob json.RawMessage) string {
	var v struct{ Tag string }
	_ = json.Unmarshal(ob, &v)
	return v.Tag
}

func union(a, b map[string]json.RawMessage) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// jsonEqual compares two JSON values ignoring formatting.
func jsonEqual(a, b json.RawMessage) bool {
	var x, y bytes.Buffer
	if json.Compact(&x, a) != nil || json.Compact(&y, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(x.Bytes(), y.Bytes())
}

// apiCmd runs `xray api <cmd>` against the running core, with doc written to
// a temporary config file when given.
func (a *Adapter) apiCmd(ctx context.Context, cmd string, doc any, args ...string) error {
	argv := []string{"api", cmd, "-s", a.opt.APIListen, "-t", "10"}
	if doc != nil {
		b, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		f, err := core.WriteTemp(a.opt.WorkDir, "xray-hot-*.json", b)
		if err != nil {
			return err
		}
		defer os.Remove(f)
		argv = append(argv, f)
	}
	argv = append(argv, args...)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, a.opt.BinPath, argv...)
	if a.proc != nil {
		c.Env = append(os.Environ(), a.proc.Env...)
	}
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("xray api %s: %v: %s", cmd, err, bytes.TrimSpace(out))
	}
	return nil
}
