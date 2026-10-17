package panel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Core DNS settings (one global config, optional per-node overrides) and rule
// sets: named domain / IP lists referenced by routing rules and DNS servers,
// optionally refreshed from a remote list. The panel expands both into each
// node's spec, so every node gets the same rules.

// dnsSpec returns the resolver config a node gets, or nil for core defaults.
func (s *Server) dnsSpec(nodeID string) *model.DNSSpec {
	var cfg store.DNSConfig
	if s.st.DB.First(&cfg, "scope = ?", nodeID).Error != nil || cfg.Mode == "inherit" || cfg.Mode == "" {
		if s.st.DB.First(&cfg, "scope = ?", model.GlobalNodeID).Error != nil || cfg.Mode != "on" {
			return nil
		}
	} else if cfg.Mode != "custom" {
		return nil // "off" on this node
	}
	d := cfg.Spec.V
	sets := s.ruleSetsByTag()
	var tags []string
	s.st.DB.Model(&store.Outbound{}).Where("node_id = ?", nodeID).Pluck("tag", &tags)
	have := map[string]bool{"direct": true, "block": true}
	for _, t := range tags {
		have[t] = true
	}
	servers := make([]model.DNSServer, 0, len(d.Servers))
	for _, sv := range d.Servers {
		if sv.Detour != "" && !have[sv.Detour] {
			sv.Detour = "" // this node lacks that egress: query directly
		}
		if len(sv.Domains) > 0 {
			sv.Domains = expandDomainRefs(sv.Domains, sets)
			if len(sv.Domains) == 0 {
				continue // only empty rule sets: the server would answer everything
			}
		}
		servers = append(servers, sv)
	}
	d.Servers = servers
	if d.FakeIP != nil && len(d.FakeIP.Domains) > 0 {
		f := *d.FakeIP
		f.Domains = expandDomainRefs(f.Domains, sets)
		if len(f.Domains) == 0 {
			f.Domains = []string{"full:fakeip.invalid"} // keep "only these names" meaning
		}
		d.FakeIP = &f
	}
	if d.DefaultServer() == nil {
		return nil
	}
	return &d
}

// expandDomainRefs replaces "ruleset:<tag>" selectors with the set's domains.
func expandDomainRefs(sel []string, sets map[string]*store.RuleSet) []string {
	var out []string
	for _, v := range sel {
		tag, ok := strings.CutPrefix(v, "ruleset:")
		if !ok {
			out = append(out, v)
			continue
		}
		rs := sets[tag]
		if rs == nil {
			continue
		}
		for _, d := range rs.Domain.V {
			out = append(out, "full:"+d)
		}
		for _, d := range rs.DomainSuffix.V {
			out = append(out, "domain:"+d)
		}
		for _, d := range rs.DomainKeyword.V {
			out = append(out, "keyword:"+d)
		}
	}
	return out
}

func (s *Server) ruleSetsByTag() map[string]*store.RuleSet {
	var all []store.RuleSet
	s.st.DB.Find(&all)
	m := make(map[string]*store.RuleSet, len(all))
	for i := range all {
		m[all[i].Tag] = &all[i]
	}
	return m
}

// expandRuleSets turns a rule that names rule sets into plain rules: one
// matching the sets' domains and one their IP ranges (a domain and an IP
// condition in one rule would both have to match). A rule whose sets are
// empty matches nothing and is dropped, never widened to a catch-all.
func expandRuleSets(r model.RouteRule, sets map[string]*store.RuleSet) []model.RouteRule {
	if len(r.Match.RuleSets) == 0 {
		return []model.RouteRule{r}
	}
	var full, suffix, kw, cidr []string
	for _, tag := range r.Match.RuleSets {
		if rs := sets[tag]; rs != nil {
			full = append(full, rs.Domain.V...)
			suffix = append(suffix, rs.DomainSuffix.V...)
			kw = append(kw, rs.DomainKeyword.V...)
			cidr = append(cidr, rs.IPCIDR.V...)
		}
	}
	base := r
	base.Match.RuleSets = nil
	var out []model.RouteRule
	if len(full)+len(suffix)+len(kw) > 0 {
		d := base
		d.ID = r.ID + "#d"
		d.Match.Domain = append(append([]string(nil), base.Match.Domain...), full...)
		d.Match.DomainSuffix = append(append([]string(nil), base.Match.DomainSuffix...), suffix...)
		d.Match.DomainKeyword = append(append([]string(nil), base.Match.DomainKeyword...), kw...)
		out = append(out, d)
	}
	if len(cidr) > 0 {
		i := base
		i.ID = r.ID + "#i"
		i.Match.IPCIDR = append(append([]string(nil), base.Match.IPCIDR...), cidr...)
		out = append(out, i)
	}
	return out
}

// --- DNS API ---

func (s *Server) getDNS(c *gin.Context) {
	var all []store.DNSConfig
	s.st.DB.Find(&all)
	out := gin.H{"global": store.DNSConfig{Scope: model.GlobalNodeID, Mode: "off"}, "nodes": []store.DNSConfig{}}
	nodes := []store.DNSConfig{}
	for _, d := range all {
		if d.Scope == model.GlobalNodeID {
			out["global"] = d
		} else {
			nodes = append(nodes, d)
		}
	}
	out["nodes"] = nodes
	c.JSON(http.StatusOK, out)
}

func (s *Server) putDNS(c *gin.Context) {
	scope := c.Param("scope")
	var req struct {
		Mode string        `json:"mode"`
		Spec model.DNSSpec `json:"spec"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	global := scope == model.GlobalNodeID
	if global {
		if req.Mode != "on" && req.Mode != "off" {
			badRequest(c, errors.New("mode 应为 on 或 off"))
			return
		}
	} else {
		var n int64
		s.st.DB.Model(&store.Node{}).Where("id = ?", scope).Count(&n)
		if n == 0 {
			notFound(c)
			return
		}
		if req.Mode != "inherit" && req.Mode != "custom" && req.Mode != "off" {
			badRequest(c, errors.New("mode 应为 inherit、custom 或 off"))
			return
		}
	}
	if req.Mode == "on" || req.Mode == "custom" {
		if err := s.checkDNSSpec(scope, &req.Spec); err != nil {
			badRequest(c, err)
			return
		}
	}
	if !global && req.Mode == "inherit" {
		s.st.DB.Delete(&store.DNSConfig{}, "scope = ?", scope)
	} else if err := s.st.DB.Save(&store.DNSConfig{Scope: scope, Mode: req.Mode, Spec: store.NewJSON(req.Spec)}).Error; err != nil {
		serverError(c, err)
		return
	}
	if global {
		s.MarkAllDirty("dns changed")
	} else {
		s.MarkDirty("dns changed", scope)
	}
	s.auditCtx(c, "dns.update", scope, req.Mode)
	s.getDNS(c)
}

func (s *Server) checkDNSSpec(scope string, d *model.DNSSpec) error {
	if err := d.Validate(); err != nil {
		return err
	}
	sets := s.ruleSetsByTag()
	var tags []string
	q := s.st.DB.Model(&store.Outbound{})
	if scope != model.GlobalNodeID {
		q = q.Where("node_id = ?", scope)
	}
	q.Pluck("tag", &tags)
	valid := map[string]bool{"direct": true, "block": true}
	for _, t := range tags {
		valid[t] = true
	}
	check := func(sel []string) error {
		for _, v := range sel {
			if tag, ok := strings.CutPrefix(v, "ruleset:"); ok && sets[tag] == nil {
				return fmt.Errorf("规则集 %q 不存在", tag)
			}
		}
		return nil
	}
	for _, sv := range d.Servers {
		if sv.Detour != "" && !valid[sv.Detour] {
			return fmt.Errorf("DNS 服务器 %s 的出站 %q 不存在", sv.Tag, sv.Detour)
		}
		if err := check(sv.Domains); err != nil {
			return err
		}
	}
	if d.FakeIP != nil {
		return check(d.FakeIP.Domains)
	}
	return nil
}

// --- rule set API ---

type ruleSetView struct {
	store.RuleSet
	Count int      `json:"count"`
	Used  []string `json:"used"` // where it is referenced
}

func (s *Server) listRuleSets(c *gin.Context) {
	var all []store.RuleSet
	s.st.DB.Order("tag").Find(&all)
	refs := s.ruleSetRefs()
	out := make([]ruleSetView, 0, len(all))
	for _, rs := range all {
		out = append(out, ruleSetView{RuleSet: rs, Count: ruleSetSize(&rs), Used: refs[rs.Tag]})
	}
	c.JSON(http.StatusOK, out)
}

func ruleSetSize(rs *store.RuleSet) int {
	return len(rs.Domain.V) + len(rs.DomainSuffix.V) + len(rs.DomainKeyword.V) + len(rs.IPCIDR.V)
}

// ruleSetRefs maps a rule set tag to the places that use it.
func (s *Server) ruleSetRefs() map[string][]string {
	out := map[string][]string{}
	var rules []store.RoutingRule
	s.st.DB.Find(&rules)
	seen := map[string]bool{}
	addRef := func(tag, where string) {
		if !seen[tag+"\x00"+where] {
			seen[tag+"\x00"+where] = true
			out[tag] = append(out[tag], where)
		}
	}
	for _, r := range rules {
		for _, t := range r.Match.V.RuleSets {
			if r.NodeID == model.GlobalNodeID {
				addRef(t, "全局路由")
			} else {
				addRef(t, "节点路由")
			}
		}
	}
	var dns []store.DNSConfig
	s.st.DB.Find(&dns)
	for _, d := range dns {
		var sel []string
		for _, sv := range d.Spec.V.Servers {
			sel = append(sel, sv.Domains...)
		}
		if d.Spec.V.FakeIP != nil {
			sel = append(sel, d.Spec.V.FakeIP.Domains...)
		}
		for _, v := range sel {
			if t, ok := strings.CutPrefix(v, "ruleset:"); ok {
				addRef(t, "DNS")
			}
		}
	}
	return out
}

var ruleSetTagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type ruleSetReq struct {
	Tag           string   `json:"tag"`
	Name          string   `json:"name"`
	Remark        string   `json:"remark"`
	SourceURL     string   `json:"sourceUrl"`
	Domain        []string `json:"domain"`
	DomainSuffix  []string `json:"domainSuffix"`
	DomainKeyword []string `json:"domainKeyword"`
	IPCIDR        []string `json:"ipCidr"`
}

func (r *ruleSetReq) apply(rs *store.RuleSet) error {
	r.Tag = strings.TrimSpace(r.Tag)
	if !ruleSetTagRe.MatchString(r.Tag) {
		return errors.New("标签只能包含小写字母、数字、- 和 _")
	}
	if r.SourceURL != "" {
		u, err := url.Parse(r.SourceURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("远程地址应以 http(s):// 开头")
		}
	}
	cidrs, err := normCIDRs(r.IPCIDR)
	if err != nil {
		return err
	}
	rs.Tag, rs.Name, rs.Remark, rs.SourceURL = r.Tag, strings.TrimSpace(r.Name), r.Remark, strings.TrimSpace(r.SourceURL)
	if rs.SourceURL == "" {
		// Manual lists are edited here; remote ones keep what the last fetch got.
		rs.Domain = store.NewJSON(cleanList(r.Domain))
		rs.DomainSuffix = store.NewJSON(cleanList(r.DomainSuffix))
		rs.DomainKeyword = store.NewJSON(cleanList(r.DomainKeyword))
		rs.IPCIDR = store.NewJSON(cidrs)
	}
	return nil
}

func cleanList(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		v = strings.TrimPrefix(v, "+.")
		v = strings.TrimPrefix(v, ".")
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func normCIDRs(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		p, err := parseCIDR(v)
		if err != nil {
			return nil, fmt.Errorf("IP 段无效：%s", v)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

func parseCIDR(v string) (string, error) {
	if p, err := netip.ParsePrefix(v); err == nil {
		return p.Masked().String(), nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil {
		return "", err
	}
	return netip.PrefixFrom(a, a.BitLen()).String(), nil
}

func (s *Server) createRuleSet(c *gin.Context) {
	var req ruleSetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	rs := store.RuleSet{ID: store.NewID()}
	if err := req.apply(&rs); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.st.DB.Create(&rs).Error; err != nil {
		badRequest(c, fmt.Errorf("标签 %s 已存在", rs.Tag))
		return
	}
	if rs.SourceURL != "" {
		s.refreshRuleSet(c.Request.Context(), &rs)
	}
	s.auditCtx(c, "ruleset.create", rs.Tag, "")
	c.JSON(http.StatusOK, rs)
}

func (s *Server) updateRuleSet(c *gin.Context) {
	var rs store.RuleSet
	if s.st.DB.First(&rs, "id = ?", c.Param("id")).Error != nil {
		notFound(c)
		return
	}
	var req ruleSetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	oldTag, oldURL := rs.Tag, rs.SourceURL
	if err := req.apply(&rs); err != nil {
		badRequest(c, err)
		return
	}
	if rs.Tag != oldTag && len(s.ruleSetRefs()[oldTag]) > 0 {
		badRequest(c, fmt.Errorf("规则集 %s 正在使用，不能改标签", oldTag))
		return
	}
	if err := s.st.DB.Save(&rs).Error; err != nil {
		badRequest(c, fmt.Errorf("标签 %s 已存在", rs.Tag))
		return
	}
	if rs.SourceURL != "" && rs.SourceURL != oldURL {
		s.refreshRuleSet(c.Request.Context(), &rs)
	} else {
		s.MarkAllDirty("rule set changed")
	}
	s.auditCtx(c, "ruleset.update", rs.Tag, "")
	c.JSON(http.StatusOK, rs)
}

func (s *Server) deleteRuleSet(c *gin.Context) {
	var rs store.RuleSet
	if s.st.DB.First(&rs, "id = ?", c.Param("id")).Error != nil {
		notFound(c)
		return
	}
	if used := s.ruleSetRefs()[rs.Tag]; len(used) > 0 {
		badRequest(c, fmt.Errorf("规则集 %s 正在被 %s 使用，请先移除引用", rs.Tag, strings.Join(used, "、")))
		return
	}
	s.st.DB.Delete(&rs)
	s.auditCtx(c, "ruleset.delete", rs.Tag, "")
	c.Status(http.StatusNoContent)
}

func (s *Server) refreshRuleSetHandler(c *gin.Context) {
	var rs store.RuleSet
	if s.st.DB.First(&rs, "id = ?", c.Param("id")).Error != nil {
		notFound(c)
		return
	}
	if rs.SourceURL == "" {
		badRequest(c, errors.New("这个规则集没有远程地址"))
		return
	}
	s.refreshRuleSet(c.Request.Context(), &rs)
	c.JSON(http.StatusOK, ruleSetView{RuleSet: rs, Count: ruleSetSize(&rs), Used: s.ruleSetRefs()[rs.Tag]})
}

// refreshRemoteRuleSets is the daily job.
func (s *Server) refreshRemoteRuleSets() {
	var all []store.RuleSet
	s.st.DB.Where("source_url <> ''").Find(&all)
	for i := range all {
		s.refreshRuleSet(context.Background(), &all[i])
	}
}

// refreshRuleSet fetches a remote list into rs and republishes on change.
func (s *Server) refreshRuleSet(ctx context.Context, rs *store.RuleSet) {
	now := time.Now()
	rs.FetchedAt = &now
	parsed, err := fetchRuleList(ctx, rs.SourceURL)
	if err != nil {
		rs.FetchError = err.Error()
		s.st.DB.Model(rs).Select("FetchedAt", "FetchError").Updates(rs)
		s.log.Warn("rule set fetch", "tag", rs.Tag, "err", err)
		return
	}
	changed := !equalStrings(rs.Domain.V, parsed.Full) || !equalStrings(rs.DomainSuffix.V, parsed.Suffix) ||
		!equalStrings(rs.DomainKeyword.V, parsed.Keyword) || !equalStrings(rs.IPCIDR.V, parsed.CIDR)
	rs.Domain, rs.DomainSuffix = store.NewJSON(parsed.Full), store.NewJSON(parsed.Suffix)
	rs.DomainKeyword, rs.IPCIDR = store.NewJSON(parsed.Keyword), store.NewJSON(parsed.CIDR)
	rs.FetchError = ""
	err = s.st.DB.Session(&gorm.Session{}).Model(rs).
		Select("Domain", "DomainSuffix", "DomainKeyword", "IPCIDR", "FetchedAt", "FetchError").Updates(rs).Error
	if err == nil && changed {
		s.MarkAllDirty("rule set refreshed")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type ruleList struct {
	Full, Suffix, Keyword, CIDR []string
}

const ruleListMax = 200000

// fetchRuleList downloads and parses a remote list.
func fetchRuleList(ctx context.Context, src string) (*ruleList, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载返回 %s", resp.Status)
	}
	return parseRuleList(io.LimitReader(resp.Body, 32<<20))
}

// parseRuleList understands the common list formats: plain domains (one per
// line, a suffix match), v2fly-style "full:" / "domain:" / "keyword:",
// Clash classical rules ("DOMAIN-SUFFIX,a.com", "IP-CIDR,1.0.0.0/8,no-resolve"),
// Clash domain / ipcidr payloads ("+.a.com", "- '1.2.3.0/24'") and bare IPs
// or CIDRs. Comments (#, //, !) and unknown lines are skipped.
func parseRuleList(r io.Reader) (*ruleList, error) {
	l := &ruleList{}
	seen := map[string]bool{}
	add := func(dst *[]string, kind, v string) {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || seen[kind+v] {
			return
		}
		seen[kind+v] = true
		*dst = append(*dst, v)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line == "payload:" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "!") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
		line = strings.Trim(line, `'"`)
		if i := strings.Index(line, " #"); i > 0 {
			line = strings.TrimSpace(line[:i])
		}
		if n++; n > ruleListMax {
			return nil, fmt.Errorf("列表超过 %d 条", ruleListMax)
		}
		if k, v, ok := strings.Cut(line, ","); ok {
			v, _, _ = strings.Cut(v, ",")
			switch strings.ToUpper(strings.TrimSpace(k)) {
			case "DOMAIN":
				add(&l.Full, "f", v)
			case "DOMAIN-SUFFIX":
				add(&l.Suffix, "s", v)
			case "DOMAIN-KEYWORD":
				add(&l.Keyword, "k", v)
			case "IP-CIDR", "IP-CIDR6":
				if p, err := parseCIDR(strings.TrimSpace(v)); err == nil {
					add(&l.CIDR, "c", p)
				}
			}
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok && !strings.Contains(v, ":") {
			switch k {
			case "full":
				add(&l.Full, "f", v)
				continue
			case "domain":
				add(&l.Suffix, "s", v)
				continue
			case "keyword":
				add(&l.Keyword, "k", v)
				continue
			case "regexp", "include", "geosite", "geoip":
				continue
			}
		}
		if p, err := parseCIDR(line); err == nil {
			add(&l.CIDR, "c", p)
			continue
		}
		line, _, _ = strings.Cut(line, " ")  // v2fly attributes: "a.com @cn"
		line, _, _ = strings.Cut(line, "\t") // hosts-style trailing columns
		switch {
		case strings.HasPrefix(line, "+."), strings.HasPrefix(line, "*."):
			add(&l.Suffix, "s", line[2:])
		case strings.HasPrefix(line, "."):
			add(&l.Suffix, "s", line[1:])
		case strings.Contains(line, "*"):
			// wildcards other than a leading label aren't expressible
		case strings.Contains(line, ".") && !strings.ContainsAny(line, "/:@"):
			add(&l.Suffix, "s", line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(l.Full)+len(l.Suffix)+len(l.Keyword)+len(l.CIDR) == 0 {
		return nil, errors.New("列表里没有可识别的域名或 IP")
	}
	for _, s := range [][]string{l.Full, l.Suffix, l.Keyword, l.CIDR} {
		sort.Strings(s)
	}
	return l, nil
}
