package realityscan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
}

// ShodanResult is what a discovery found.
type ShodanResult struct {
	Query string   `json:"query"`
	Total int      `json:"total"` // matches Shodan has for the query
	Hosts []string `json:"hosts"` // host:port, names preferred over IPs, deduplicated
}

type shodanMatch struct {
	IP        string   `json:"ip_str"`
	Port      int      `json:"port"`
	Hostnames []string `json:"hostnames"`
	SSL       *struct {
		Cert struct {
			Subject struct {
				CN string `json:"CN"`
			} `json:"subject"`
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
	out := &ShodanResult{Query: query, Hosts: []string{}}
	seen := map[string]bool{}
	for page := 1; page <= pages; page++ {
		var body struct {
			Total   int           `json:"total"`
			Matches []shodanMatch `json:"matches"`
		}
		v := url.Values{"key": {key}, "query": {query}, "page": {strconv.Itoa(page)}}
		if err := shodanGet(ctx, key, "/shodan/host/search", v, &body); err != nil {
			if page > 1 && len(out.Hosts) > 0 {
				break // keep what the earlier pages found
			}
			return nil, err
		}
		out.Total = body.Total
		for _, m := range body.Matches {
			if h := candidateOf(m); h != "" && !seen[strings.ToLower(h)] {
				seen[strings.ToLower(h)] = true
				out.Hosts = append(out.Hosts, h)
			}
		}
		if len(body.Matches) < 100 {
			break // last page
		}
	}
	return out, nil
}

// candidateOf picks the name a match is probed by: the certificate name
// (it is what REALITY clients send as SNI), else a reverse-DNS name, else
// the address (Probe then reads names from the certificate).
func candidateOf(m shodanMatch) string {
	port := m.Port
	if port == 0 {
		port = 443
	}
	host := ""
	if m.SSL != nil {
		if cn := strings.ToLower(strings.TrimSpace(m.SSL.Cert.Subject.CN)); validHostname(cn) {
			host = cn
		}
	}
	if host == "" {
		for _, h := range m.Hostnames {
			if h = strings.ToLower(strings.TrimSpace(h)); validHostname(h) {
				host = h
				break
			}
		}
	}
	if host == "" {
		if net.ParseIP(m.IP) == nil {
			return ""
		}
		host = m.IP
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
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
