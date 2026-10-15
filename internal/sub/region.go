package sub

import (
	"regexp"
	"strings"
)

// Region describes a well-known exit region and how node names refer to it.
type Region struct {
	Code string
	Zh   string
	En   string
	Rank int
	re   *regexp.Regexp
}

var regions = func() []Region {
	// zh: Chinese keywords; en: English names (case-insensitive); codes are
	// matched case-sensitively and not inside other Latin words ("HK01" works,
	// "think" does not).
	defs := []struct{ code, zh, en, extra string }{
		{"HK", "香港|港", "hong ?kong", ""},
		{"TW", "台湾|台灣|台北|新北|彰化", "taiwan", ""},
		{"JP", "日本|东京|東京|大阪|埼玉", "japan|tokyo|osaka", ""},
		{"SG", "新加坡|狮城|獅城", "singapore", ""},
		{"US", "美国|美國|洛杉矶|圣何塞|西雅图|纽约|硅谷", "united states|america|los angeles|seattle|san jose", "USA"},
		{"KR", "韩国|韓國|首尔|首爾|春川", "korea|seoul", ""},
		{"GB", "英国|英國|伦敦", "united kingdom|britain|london", "UK"},
		{"DE", "德国|德國|法兰克福", "germany|frankfurt", ""},
		{"FR", "法国|法國|巴黎", "france|paris", ""},
		{"NL", "荷兰|荷蘭|阿姆斯特丹", "netherlands|amsterdam", ""},
		{"CA", "加拿大|多伦多|温哥华", "canada|toronto|vancouver", ""},
		{"AU", "澳大利亚|澳洲|悉尼", "australia|sydney", ""},
		{"RU", "俄罗斯|俄羅斯|莫斯科", "russia|moscow", ""},
		{"IN", "印度|孟买", "india|mumbai", ""},
		{"TR", "土耳其|伊斯坦布尔", "turkey|türkiye|istanbul", ""},
		{"MY", "马来西亚|馬來西亞|吉隆坡", "malaysia|kuala lumpur", ""},
		{"TH", "泰国|泰國|曼谷", "thailand|bangkok", ""},
		{"VN", "越南|胡志明", "vietnam", ""},
		{"PH", "菲律宾|菲律賓|马尼拉", "philippines|manila", ""},
		{"AR", "阿根廷", "argentina", ""},
		{"BR", "巴西|圣保罗", "brazil|sao paulo", ""},
		{"AE", "阿联酋|迪拜", "dubai|emirates", "UAE"},
	}
	out := make([]Region, len(defs))
	for i, d := range defs {
		codes := d.code
		if d.extra != "" {
			codes += "|" + d.extra
		}
		pat := d.zh + `|(?i:` + d.en + `)|(?:^|[^A-Za-z])(?:` + codes + `)(?:[^A-Za-z]|$)|` + Flag(d.code)
		out[i] = Region{Code: d.code, Zh: d.zh[:strings.Index(d.zh+"|", "|")], En: titleFirst(d.en), Rank: i, re: regexp.MustCompile(pat)}
	}
	return out
}()

var regionByCode = func() map[string]Region {
	m := map[string]Region{}
	for _, r := range regions {
		m[r.Code] = r
	}
	return m
}()

// Regions lists the built-in regions (for UIs).
func Regions() []Region { return regions }

// NodeRegion returns the ISO code of a node: its explicit Region, else a
// detection from its name. Unknown returns "".
func NodeRegion(n Node) string {
	if n.Region != "" {
		return strings.ToUpper(n.Region)
	}
	return DetectRegion(n.Name)
}

// DetectRegion guesses a region code from a node name.
func DetectRegion(name string) string {
	for _, r := range regions {
		if r.re.MatchString(name) {
			return r.Code
		}
	}
	return ""
}

// RegionGroup is a per-region list of node names, offered to templates for
// building region policy groups.
type RegionGroup struct {
	Code      string
	Name      string   // "🇭🇰 香港"
	Names     []string // member node names
	NamesJSON string   // JSON/YAML flow list of member names
}

// GroupByRegion buckets nodes in display order; only non-empty regions are returned.
func GroupByRegion(nodes []Node) []RegionGroup {
	byCode := map[string][]string{}
	for _, n := range nodes {
		if c := NodeRegion(n); c != "" {
			byCode[c] = append(byCode[c], n.Name)
		}
	}
	var out []RegionGroup
	for _, r := range regions {
		if names, ok := byCode[r.Code]; ok {
			out = append(out, RegionGroup{Code: r.Code, Name: strings.TrimSpace(Flag(r.Code) + " " + r.Zh), Names: names, NamesJSON: flowList(names)})
		}
	}
	return out
}

func titleFirst(alts string) string {
	first := alts
	if i := strings.Index(alts, "|"); i >= 0 {
		first = alts[:i]
	}
	first = strings.ReplaceAll(first, " ?", " ")
	words := strings.Fields(first)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
