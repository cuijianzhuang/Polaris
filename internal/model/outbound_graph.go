package model

import (
	"fmt"
	"strings"
)

// OutboundDependencies includes every edge that can re-enter another egress.
// Checking only detour edges misses cycles such as A -> fallback B -> member C
// -> detour A.
func (o Outbound) OutboundDependencies() ([]string, error) {
	s, err := o.ParseSettings()
	if err != nil {
		return nil, fmt.Errorf("outbound %s: %w", o.Tag, err)
	}
	var deps []string
	if o.Detour != "" {
		deps = append(deps, o.Detour)
	}
	if o.Type.IsGroup() {
		deps = append(deps, s.Members...)
		if s.Fallback != "" {
			deps = append(deps, s.Fallback)
		}
	}
	return deps, nil
}

// ValidateOutboundGraph rejects dangling references and cycles before a spec
// reaches a core. Fallback is deliberately separate from the primary members.
func ValidateOutboundGraph(obs []Outbound) error {
	byTag := map[string]Outbound{}
	for _, o := range obs {
		if strings.TrimSpace(o.Tag) == "" {
			return fmt.Errorf("出站标签不能为空")
		}
		if _, exists := byTag[o.Tag]; exists {
			return fmt.Errorf("重复出站标签 %q", o.Tag)
		}
		byTag[o.Tag] = o
	}
	for _, o := range obs {
		s, err := o.ParseSettings()
		if err != nil {
			return fmt.Errorf("outbound %s: %w", o.Tag, err)
		}
		if o.Type.IsGroup() {
			if len(s.Members) == 0 {
				return fmt.Errorf("均衡组 %q 至少需要一个成员", o.Tag)
			}
			seen := map[string]bool{}
			for _, m := range s.Members {
				if seen[m] || m == "" {
					return fmt.Errorf("均衡组 %q 有重复或空成员", o.Tag)
				}
				seen[m] = true
				if member, exists := byTag[m]; exists && member.Type.IsGroup() {
					return fmt.Errorf("均衡组 %q 的成员 %q 也是组；请通过兜底配置备用组", o.Tag, m)
				}
			}
			if seen[s.Fallback] {
				return fmt.Errorf("均衡组 %q 的兜底不能同时是主组成员", o.Tag)
			}
		}
	}
	state := map[string]int{}
	var visit func(string, []string) error
	visit = func(tag string, path []string) error {
		if state[tag] == 1 {
			return fmt.Errorf("出站引用成环：%s", strings.Join(append(path, tag), " → "))
		}
		if state[tag] == 2 {
			return nil
		}
		o, exists := byTag[tag]
		if !exists {
			if tag == "direct" || tag == "block" {
				return nil
			}
			return fmt.Errorf("出站 %q 不存在", tag)
		}
		state[tag] = 1
		deps, err := o.OutboundDependencies()
		if err != nil {
			return err
		}
		for _, next := range deps {
			if err := visit(next, append(path, tag)); err != nil {
				return err
			}
		}
		state[tag] = 2
		return nil
	}
	for _, o := range obs {
		if err := visit(o.Tag, nil); err != nil {
			return err
		}
	}
	return nil
}
