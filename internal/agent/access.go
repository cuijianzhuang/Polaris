package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

const accessTable = "polaris_access"

func (f *firewall) accessState() ([]model.AccessRule, error) {
	b, e := os.ReadFile(f.state + ".access.json")
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var rules []model.AccessRule
	if e = json.Unmarshal(b, &rules); e != nil {
		return nil, e
	}
	return model.NormalizeAccessRules(rules)
}

func renderAccess(rules []model.AccessRule, exists bool) string {
	var b strings.Builder
	if exists {
		fmt.Fprintf(&b, "delete table inet %s\n", accessTable)
	}
	if len(rules) == 0 {
		return b.String()
	}
	fmt.Fprintf(&b, "table inet %s {\n chain input {\n  type filter hook input priority -10; policy accept;\n  iifname \"lo\" accept\n", accessTable)
	for _, r := range rules {
		for _, family := range []string{"ip", "ip6"} {
			var sources []string
			for _, source := range r.Sources {
				p, _ := netip.ParsePrefix(source)
				if p.Addr().Is4() == (family == "ip") {
					sources = append(sources, source)
				}
			}
			if len(sources) > 0 {
				fmt.Fprintf(&b, "  %s dport %d %s saddr { %s } accept\n", r.Protocol, r.Port, family, strings.Join(sources, ", "))
			}
		}
		fmt.Fprintf(&b, "  %s dport %d drop\n", r.Protocol, r.Port)
	}
	b.WriteString(" }\n}\n")
	return b.String()
}

// syncAccess uses its own inet table: unrelated firewall rules are untouched.
// nft validates and commits the complete replacement as one kernel transaction.
func (f *firewall) syncAccess(ctx context.Context, rules []model.AccessRule) error {
	rules, e := model.NormalizeAccessRules(rules)
	if e != nil {
		return e
	}
	owned, e := f.accessState()
	if e != nil {
		return fmt.Errorf("read access state: %w", e)
	}
	if len(rules) == 0 && len(owned) == 0 {
		return nil
	}
	if !f.look("nft") {
		return fmt.Errorf("中转来源限制需要 nftables；请先安装 nft 并授予 NET_ADMIN 权限")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, e := f.run(ctx, "nft", "list", "table", "inet", accessTable)
	exists := e == nil
	if e != nil && !strings.Contains(out, "No such file") && !strings.Contains(out, "does not exist") {
		return fmt.Errorf("检查中转来源规则失败：%s: %w", strings.TrimSpace(out), e)
	}
	script := renderAccess(rules, exists)
	if script != "" {
		if e = os.MkdirAll(filepath.Dir(f.state), 0700); e != nil {
			return e
		}
		tmp, e := os.CreateTemp(filepath.Dir(f.state), "access-*.nft")
		if e != nil {
			return e
		}
		path := tmp.Name()
		defer os.Remove(path)
		_, writeErr := tmp.WriteString(script)
		closeErr := tmp.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		if out, e = f.run(ctx, "nft", "--check", "--file", path); e != nil {
			return fmt.Errorf("中转来源规则校验失败：%s: %w", strings.TrimSpace(out), e)
		}
		if out, e = f.run(ctx, "nft", "--file", path); e != nil {
			return fmt.Errorf("中转来源规则应用失败（原规则保留）：%s: %w", strings.TrimSpace(out), e)
		}
	}
	b, _ := json.Marshal(rules)
	return core.AtomicWriteDurable(f.state+".access.json", b, 0600)
}

func (a *Agent) prepareAccess(ctx context.Context, spec *model.NodeSpec) error {
	if a.fw == nil {
		if len(spec.AccessRules) > 0 {
			return fmt.Errorf("source restriction enforcement unavailable")
		}
		return nil
	}
	old, e := a.fw.accessState()
	if e != nil {
		return e
	}
	guard, e := model.TransitionalAccess(old, spec.AccessRules)
	if e != nil {
		return e
	}
	if len(guard) == 0 {
		return nil
	}
	return a.fw.syncAccess(ctx, guard)
}
