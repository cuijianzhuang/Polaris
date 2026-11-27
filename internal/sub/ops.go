package sub

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Operator chain: nodes flow through an ordered list of operators
// (dedup → filter → rename → sort is the usual order).

// Operator is one processing step.
type Operator struct {
	Type     string `json:"type"` // filter / rename / sort / dedup
	Disabled bool   `json:"disabled,omitempty"`

	// filter
	Protocols []string `json:"protocols,omitempty"` // keep only these protocols
	Regions   []string `json:"regions,omitempty"`   // keep only these region codes (HK, JP…)
	Include   string   `json:"include,omitempty"`   // keep names matching this regex
	Exclude   string   `json:"exclude,omitempty"`   // drop names matching this regex
	DropInfo  bool     `json:"dropInfo,omitempty"`  // drop pseudo nodes ("剩余流量", "到期时间", "官网"…)
	// View-group predicate dimensions (panel nodes only): endpoint kind
	// (direct / cdn / relay), relay chain id whitelist, traffic-rate ceiling
	// and inbound tag whitelist. Empty / nil = unconstrained.
	Kinds    []string `json:"kinds,omitempty"`
	ChainIDs []string `json:"chainIds,omitempty"`
	MaxRate  *float64 `json:"maxRate,omitempty"`
	Tags     []string `json:"tags,omitempty"`

	// rename
	Pattern  string `json:"pattern,omitempty"`  // regex
	Replace  string `json:"replace,omitempty"`  // replacement ($1 supported)
	Template string `json:"template,omitempty"` // e.g. "{emoji} {regionZh} {protocol} {index}"

	// sort: keys applied in order: region / protocol / name / server / rate
	SortBy      []string `json:"sortBy,omitempty"`
	RegionOrder []string `json:"regionOrder,omitempty"` // custom region priority
	Desc        bool     `json:"desc,omitempty"`
}

var infoNodeRe = regexp.MustCompile(`(?i)剩余|流量|到期|过期|官网|重置|expire|traffic|官方|套餐|网址|客服|Telegram频道`)

// reCache memoises compiled patterns: chains are validated (compiled) when
// saved and re-run on every subscription render, so execution must not pay
// regexp.Compile per request. Patterns are administrator-authored and few.
var reCache sync.Map // string -> *regexp.Regexp

func compileRe(pattern string) (*regexp.Regexp, error) {
	if v, ok := reCache.Load(pattern); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	reCache.Store(pattern, re)
	return re, nil
}

// effectiveRate is the multiplier a node is billed at; 0 means "unset" and
// counts as 1, matching the panel's expansion default.
func effectiveRate(n Node) float64 {
	if n.TrafficRate <= 0 {
		return 1
	}
	return n.TrafficRate
}

// kindLabel renders a panel endpoint kind for the rename {kind} variable.
func kindLabel(n Node) string {
	switch n.PanelKind {
	case "direct":
		return "直连"
	case "cdn":
		return "CDN"
	case "relay":
		return "中转"
	}
	return n.PanelKind
}

// ApplyChain runs the operators in order.
func ApplyChain(nodes []Node, ops []Operator) ([]Node, error) {
	out := append([]Node(nil), nodes...)
	for i, op := range ops {
		if op.Disabled {
			continue
		}
		var err error
		switch op.Type {
		case "filter":
			out, err = opFilter(out, op)
		case "rename":
			out, err = opRename(out, op)
		case "sort":
			opSort(out, op)
		case "dedup":
			out = Deduplicate(out)
		default:
			err = fmt.Errorf("unknown operator %q", op.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("operator %d (%s): %w", i+1, op.Type, err)
		}
	}
	return Dedupe(out), nil
}

// ValidateChain compiles every regex without running the chain.
func ValidateChain(ops []Operator) error {
	_, err := ApplyChain(nil, ops)
	if err != nil {
		return err
	}
	for i, op := range ops {
		for _, re := range []string{op.Include, op.Exclude, op.Pattern} {
			if re == "" {
				continue
			}
			if _, err := compileRe(re); err != nil {
				return fmt.Errorf("operator %d: %w", i+1, err)
			}
		}
		if op.MaxRate != nil && *op.MaxRate < 0 {
			return fmt.Errorf("operator %d: maxRate must not be negative", i+1)
		}
		for _, k := range op.Kinds {
			switch k {
			case "direct", "cdn", "relay":
			default:
				return fmt.Errorf("operator %d: unknown kind %q", i+1, k)
			}
		}
	}
	return nil
}

// filterPred is a compiled filter operator: regexes compiled once, lookup
// sets built once, then matched per node.
type filterPred struct {
	op                  Operator
	inc, exc            *regexp.Regexp
	protos, regions     map[string]bool
	kinds, chains, tags map[string]bool
}

func compileFilterPred(op Operator) (filterPred, error) {
	p := filterPred{op: op, protos: set(op.Protocols), regions: set(op.Regions),
		kinds: set(op.Kinds), chains: set(op.ChainIDs), tags: set(op.Tags)}
	var err error
	if op.Include != "" {
		if p.inc, err = compileRe(op.Include); err != nil {
			return p, err
		}
	}
	if op.Exclude != "" {
		if p.exc, err = compileRe(op.Exclude); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (p filterPred) match(n Node) bool {
	op := p.op
	switch {
	case len(p.protos) > 0 && !p.protos[string(n.Protocol)]:
		return false
	case len(p.regions) > 0 && !p.regions[NodeRegion(n)]:
		return false
	case len(p.kinds) > 0 && !p.kinds[n.PanelKind]:
		return false
	case len(p.chains) > 0 && !p.chains[n.PanelChainID]:
		return false
	case op.MaxRate != nil && effectiveRate(n) > *op.MaxRate:
		return false
	case len(p.tags) > 0 && !p.tags[n.PanelTag]:
		return false
	case p.inc != nil && !p.inc.MatchString(n.Name):
		return false
	case p.exc != nil && p.exc.MatchString(n.Name):
		return false
	case op.DropInfo && infoNodeRe.MatchString(n.Name):
		return false
	}
	return true
}

func opFilter(nodes []Node, op Operator) ([]Node, error) {
	p, err := compileFilterPred(op)
	if err != nil {
		return nil, err
	}
	var out []Node
	for _, n := range nodes {
		if p.match(n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// SplitByFilter partitions nodes by a filter operator used as a predicate
// (the view-group selector form): matched nodes satisfy the predicate, rest
// are the complement; both keep their original order.
func SplitByFilter(nodes []Node, pred Operator) (matched, rest []Node, err error) {
	p, err := compileFilterPred(pred)
	if err != nil {
		return nil, nil, err
	}
	for _, n := range nodes {
		if p.match(n) {
			matched = append(matched, n)
		} else {
			rest = append(rest, n)
		}
	}
	return matched, rest, nil
}

func opRename(nodes []Node, op Operator) ([]Node, error) {
	var re *regexp.Regexp
	if op.Pattern != "" {
		var err error
		if re, err = compileRe(op.Pattern); err != nil {
			return nil, err
		}
	}
	counters := map[string]int{}
	for i := range nodes {
		n := &nodes[i]
		if re != nil {
			n.Name = re.ReplaceAllString(n.Name, op.Replace)
		}
		if op.Template != "" {
			code := NodeRegion(*n)
			key := code
			counters[key]++
			r := regionByCode[code]
			n.Name = NameVars(op.Template, map[string]string{
				"emoji": Flag(code), "region": code, "regionZh": r.Zh, "regionEn": r.En,
				"protocol": ProtocolLabel(*n), "index": fmt.Sprintf("%02d", counters[key]), "name": n.Name,
				"server": n.Server, "port": strconv.Itoa(n.Port), "source": n.Source,
				"kind": kindLabel(*n), "chain": n.PanelChainName, "group": n.PanelViewGroup,
			})
		}
		n.Name = strings.TrimSpace(n.Name)
	}
	return nodes, nil
}

func opSort(nodes []Node, op Operator) {
	keys := op.SortBy
	if len(keys) == 0 {
		keys = []string{"region", "name"}
	}
	prio := map[string]int{}
	for i, r := range op.RegionOrder {
		prio[strings.ToUpper(r)] = i + 1
	}
	regionRank := func(n Node) int {
		code := NodeRegion(n)
		if p, ok := prio[code]; ok {
			return p
		}
		if r, ok := regionByCode[code]; ok {
			return 1000 + r.Rank
		}
		return 10000
	}
	cmp := func(a, b Node) int {
		for _, k := range keys {
			var c int
			switch k {
			case "region":
				c = regionRank(a) - regionRank(b)
			case "protocol":
				c = strings.Compare(string(a.Protocol), string(b.Protocol))
			case "server":
				c = strings.Compare(a.Server, b.Server)
			case "rate":
				ra, rb := effectiveRate(a), effectiveRate(b)
				if ra < rb {
					c = -1
				} else if ra > rb {
					c = 1
				}
			default:
				c = naturalCompare(a.Name, b.Name)
			}
			if c != 0 {
				return c
			}
		}
		return 0
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		c := cmp(nodes[i], nodes[j])
		if op.Desc {
			return c > 0
		}
		return c < 0
	})
}

// Deduplicate drops nodes with the same protocol, endpoint and credential.
func Deduplicate(nodes []Node) []Node {
	seen := map[string]bool{}
	var out []Node
	for _, n := range nodes {
		key := fmt.Sprintf("%s|%s|%d|%s|%s|%s", n.Protocol, strings.ToLower(n.Server), n.Port, n.UUID, n.Password, n.Transport.Path)
		if n.Protocol == "snell" {
			key += fmt.Sprintf("|%q|%q|%d|%q|%q|%t", n.SnellPSK, n.SnellUserKey, n.SnellVersion, n.SnellMode, n.SnellObfs, n.SnellSinglePSK)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	return out
}

// naturalCompare orders "HK 2" before "HK 10".
func naturalCompare(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		if isDigit(ra[i]) && isDigit(rb[j]) {
			si := i
			for i < len(ra) && isDigit(ra[i]) {
				i++
			}
			sj := j
			for j < len(rb) && isDigit(rb[j]) {
				j++
			}
			na, _ := strconv.Atoi(string(ra[si:i]))
			nb, _ := strconv.Atoi(string(rb[sj:j]))
			if na != nb {
				return na - nb
			}
			continue
		}
		if ra[i] != rb[j] {
			if ra[i] < rb[j] {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	return (len(ra) - i) - (len(rb) - j)
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func set(ss []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range ss {
		m[s] = true
		m[strings.ToUpper(s)] = true
	}
	return m
}
