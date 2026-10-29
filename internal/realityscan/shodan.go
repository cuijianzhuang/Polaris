package realityscan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Candidate discovery through Shodan's search API: Shodan's own index of
// public HTTPS servers is queried (no scanning from here), and the hosts it
// returns are then checked with Probe like any other candidate.

// ShodanBase is the API root; tests point it at a fake server.
var ShodanBase = "https://api.shodan.io"

// DefaultShodanQuery finds HTTPS servers that already advertise TLS 1.3 and
// HTTP/2, the two hard requirements of a REALITY target.
const DefaultShodanQuery = "port:443 ssl.version:tlsv1.3 ssl.alpn:h2"

// MaxShodanPages caps one discovery: every page of 100 results costs a query
// credit on filtered searches.
const MaxShodanPages = 3

// ShodanQuery is one discovery request.
type ShodanQuery struct {
	Query   string // Shodan search filters; empty: DefaultShodanQuery
	Country string // ISO 3166 code added as country:XX (optional)
	Pages   int    // 1–MaxShodanPages
	ASN     string // optional AS number
	Network string // optional exact CIDR filter, queried in Shodan only
}

// ShodanResult is what a discovery found.
type ShodanResult struct {
	Query      string      `json:"query"`
	Total      int         `json:"total"` // matches Shodan has for the query
	Hosts      []string    `json:"hosts"` // retained for API clients; actual dial addresses
	Candidates []Candidate `json:"candidates"`
	Warnings   []string    `json:"warnings,omitempty"`
	PagesRead  int         `json:"pagesRead"`
}

type shodanMatch struct {
	IP       string `json:"ip_str"`
	ASN      string `json:"asn"`
	Org      string `json:"org"`
	Location struct {
		Country string `json:"country_code"`
	} `json:"location"`
	Port      int      `json:"port"`
	Hostnames []string `json:"hostnames"`
	SSL       *struct {
		Cert struct {
			Subject struct {
				CN string `json:"CN"`
			} `json:"subject"`
			Extensions []struct {
				Name string `json:"name"`
				Data string `json:"data"`
			} `json:"extensions"`
		} `json:"cert"`
	} `json:"ssl"`
}

func (q ShodanQuery) build() (string, error) {
	query := strings.TrimSpace(q.Query)
	if query == "" {
		query = DefaultShodanQuery
	}
	if cc := strings.ToUpper(strings.TrimSpace(q.Country)); cc != "" {
		if len(cc) != 2 || cc[0] < 'A' || cc[0] > 'Z' || cc[1] < 'A' || cc[1] > 'Z' {
			return "", errors.New("国家代码应为两个字母，例如 JP")
		}
		if !strings.Contains(strings.ToLower(query), "country:") {
			query += " country:" + cc
		}
	}
	if asn := strings.ToUpper(strings.TrimSpace(q.ASN)); asn != "" {
		asn = strings.TrimPrefix(asn, "AS")
		n, err := strconv.ParseUint(asn, 10, 32)
		if err != nil || n == 0 {
			return "", errors.New("ASN 格式应为 AS13335 或 13335")
		}
		if !strings.Contains(strings.ToLower(query), "asn:") {
			query += " asn:AS" + strconv.FormatUint(n, 10)
		}
	}
	if network := strings.TrimSpace(q.Network); network != "" {
		prefix, err := netip.ParsePrefix(network)
		if err != nil {
			return "", errors.New("网段必须是有效 CIDR，例如 203.0.113.0/24")
		}
		if !strings.Contains(strings.ToLower(query), "net:") {
			query += " net:" + prefix.Masked().String()
		}
	}
	if len(query) > 500 {
		return "", errors.New("查询条件太长")
	}
	return query, nil
}

// ShodanDiscover runs a search and returns candidate targets.
func ShodanDiscover(ctx context.Context, key string, q ShodanQuery) (*ShodanResult, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("请先在设置里填写 Shodan API Key")
	}
	query, err := q.build()
	if err != nil {
		return nil, err
	}
	pages := min(max(q.Pages, 1), MaxShodanPages)
	out := &ShodanResult{Query: query, Hosts: []string{}, Candidates: []Candidate{}}
	seen := map[string]bool{}
	for page := 1; page <= pages; page++ {
		var body struct {
			Total   int           `json:"total"`
			Matches []shodanMatch `json:"matches"`
		}
		v := url.Values{"key": {key}, "query": {query}, "page": {strconv.Itoa(page)}}
		if err := shodanGet(ctx, key, "/shodan/host/search", v, &body); err != nil {
			if page > 1 && len(out.Hosts) > 0 {
				out.Warnings = append(out.Warnings, fmt.Sprintf("第 %d 页查询未完成：%s", page, err.Error()))
				break // keep what earlier pages found, with a visible explanation
			}
			return nil, err
		}
		out.PagesRead++
		out.Total = body.Total
		for _, m := range body.Matches {
			c := candidateOf(m)
			identity := c.Address + "|" + c.ServerName
			if c.Address != "" && !seen[identity] {
				seen[identity] = true
				out.Candidates = append(out.Candidates, c)
				out.Hosts = append(out.Hosts, c.Address)
			}
		}
		if len(body.Matches) < 100 {
			break // last page
		}
	}
	return out, nil
}

// candidateOf preserves Shodan's actual service IP and an independent SNI
// hint. Equal certificates on different IPs are still distinct candidates.
func candidateOf(m shodanMatch) Candidate {
	ip := net.ParseIP(m.IP)
	port := m.Port
	if port == 0 {
		port = 443
	}
	if ip == nil || port < 1 || port > 65535 {
		return Candidate{}
	}
	c := Candidate{Address: net.JoinHostPort(ip.String(), strconv.Itoa(port)), ASN: m.ASN, Org: m.Org, Country: m.Location.Country}
	var names []string
	if m.SSL != nil {
		for _, ext := range m.SSL.Cert.Extensions {
			if strings.EqualFold(ext.Name, "subjectAltName") {
				for _, entry := range strings.Split(ext.Data, ",") {
					entry = strings.TrimSpace(entry)
					if strings.HasPrefix(strings.ToUpper(entry), "DNS:") {
						names = append(names, entry[4:])
					}
				}
			}
		}
		names = append(names, m.SSL.Cert.Subject.CN)
	}
	names = append(names, m.Hostnames...)
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if validHostname(name) {
			c.ServerName = name
			break
		}
	}
	return c
}

// validHostname accepts plain DNS names (no wildcards, IPs or spaces).
func validHostname(h string) bool {
	if h == "" || len(h) > 253 || strings.HasPrefix(h, "*") || net.ParseIP(h) != nil || !strings.Contains(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// ShodanCredits returns the query credits left and the plan name.
func ShodanCredits(ctx context.Context, key string) (int, string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return 0, "", errors.New("请先在设置里填写 Shodan API Key")
	}
	var body struct {
		QueryCredits int    `json:"query_credits"`
		Plan         string `json:"plan"`
	}
	if err := shodanGet(ctx, key, "/api-info", url.Values{"key": {key}}, &body); err != nil {
		return 0, "", err
	}
	return body.QueryCredits, body.Plan, nil
}

func shodanGet(ctx context.Context, key, path string, v url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ShodanBase+path+"?"+v.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// URLs end up in errors: keep the key out.
		return fmt.Errorf("连接 Shodan 失败：%s", strings.ReplaceAll(err.Error(), key, "***"))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			return errors.New("Shodan API Key 无效")
		case resp.StatusCode == http.StatusForbidden && e.Error == "":
			return errors.New("Shodan 拒绝了请求：当前套餐可能不支持搜索过滤条件")
		case e.Error != "":
			return fmt.Errorf("Shodan：%s", strings.ReplaceAll(e.Error, key, "***"))
		}
		return fmt.Errorf("Shodan 返回 HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("Shodan 返回了无法识别的内容")
	}
	return nil
}

// ShodanIPs runs a search and returns the matching hosts' IPv4 addresses
// (in result order, without repeats) and Shodan's total match count.
func ShodanIPs(ctx context.Context, key, query string, pages int) ([]string, int, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, 0, errors.New("请先在设置里填写 Shodan API Key")
	}
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 500 {
		return nil, 0, errors.New("Shodan 查询条件不能为空，最长 500 个字符")
	}
	pages = min(max(pages, 1), MaxShodanPages)
	var ips []string
	total := 0
	seen := map[string]bool{}
	for page := 1; page <= pages; page++ {
		var body struct {
			Total   int           `json:"total"`
			Matches []shodanMatch `json:"matches"`
		}
		v := url.Values{"key": {key}, "query": {query}, "page": {strconv.Itoa(page)}, "minify": {"true"}}
		if err := shodanGet(ctx, key, "/shodan/host/search", v, &body); err != nil {
			if len(ips) > 0 {
				break // keep what earlier pages found
			}
			return nil, 0, err
		}
		total = body.Total
		for _, m := range body.Matches {
			ip := net.ParseIP(m.IP).To4()
			if ip == nil || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			ips = append(ips, ip.String())
		}
		if len(body.Matches) < 100 {
			break
		}
	}
	return ips, total, nil
}
