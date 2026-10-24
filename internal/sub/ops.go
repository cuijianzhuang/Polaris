package sub

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
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

	// rename
	Pattern  string `json:"pattern,omitempty"`  // regex
	Replace  string `json:"replace,omitempty"`  // replacement ($1 supported)
	Template string `json:"template,omitempty"` // e.g. "{emoji} {regionZh} {protocol} {index}"

	// sort: keys applied in order: region / protocol / name / server
	SortBy      []string `json:"sortBy,omitempty"`
	RegionOrder []string `json:"regionOrder,omitempty"` // custom region priority
	Desc        bool     `json:"desc,omitempty"`
}

var infoNodeRe = regexp.MustCompile(`(?i)剩余|流量|到期|过期|官网|重置|expire|traffic|官方|套餐|网址|客服|Telegram频道`)

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
			if _, err := regexp.Compile(re); err != nil {
				return fmt.Errorf("operator %d: %w", i+1, err)
			}
		}
	}
	return nil
}

func opFilter(nodes []Node, op Operator) ([]Node, error) {
	var inc, exc *regexp.Regexp
	var err error
	if op.Include != "" {
		if inc, err = regexp.Compile(op.Include); err != nil {
			return nil, err
		}
	}
	if op.Exclude != "" {
		if exc, err = regexp.Compile(op.Exclude); err != nil {
			return nil, err
		}
	}
	protos := set(op.Protocols)
	regions := set(op.Regions)
	var out []Node
	for _, n := range nodes {
		if len(protos) > 0 && !protos[string(n.Protocol)] {
			continue
		}
		if len(regions) > 0 && !regions[NodeRegion(n)] {
			continue
		}
		if inc != nil && !inc.MatchString(n.Name) {
			continue
		}
		if exc != nil && exc.MatchString(n.Name) {
			continue
		}
		if op.DropInfo && infoNodeRe.MatchString(n.Name) {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

func opRename(nodes []Node, op Operator) ([]Node, error) {
	var re *regexp.Regexp
	if op.Pattern != "" {
		var err error
		if re, err = regexp.Compile(op.Pattern); err != nil {
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
