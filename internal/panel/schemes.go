package panel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Rule schemes ("规则与配置方案"): a preset or custom subconverter-style ini
// that decides the policy groups and rules of generated client configs, for
// the built-in renderer and third-party backends alike.

const (
	schemeTTL  = 6 * time.Hour
	ruleSetTTL = 12 * time.Hour
)

type cached struct {
	at   time.Time
	sc   *sub.Scheme
	body []byte
}

// schemeCache keeps fetched remote schemes and converted rule sets.
type schemeCache struct {
	mu sync.Mutex
	m  map[string]cached
}

func (c *schemeCache) get(k string, ttl time.Duration) (cached, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok, ok && time.Since(v.at) < ttl
}

func (c *schemeCache) put(k string, v cached) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 512 {
		c.m = map[string]cached{}
	}
	v.at = time.Now()
	c.m[k] = v
}

// renderOpts are the per-subscription rendering choices.
type renderOpts struct {
	Scheme   string // "" = template default
	SkipCert bool
	ForceUDP bool
}

func (s *Server) renderOptsFor(p *store.SubProfile) renderOpts {
	ro := renderOpts{
		Scheme:   s.st.GetSetting("sub_scheme", ""),
		SkipCert: s.st.GetSetting("sub_skip_cert", "false") == "true",
		ForceUDP: s.st.GetSetting("sub_force_udp", "false") == "true",
	}
	if p != nil {
		if p.Scheme != "" {
			ro.Scheme = p.Scheme
		}
		if p.SkipCert != nil {
			ro.SkipCert = *p.SkipCert
		}
		if p.ForceUDP != nil {
			ro.ForceUDP = *p.ForceUDP
		}
	}
	if ro.Scheme == "none" {
		ro.Scheme = ""
	}
	return ro
}

// schemeSourceURL is where a third-party backend reads the scheme from.
func (s *Server) schemeSourceURL(id string) string {
	if p, ok := sub.Preset(id); ok {
		if p.URL != "" {
			return p.URL
		}
		return s.subBase() + "/sub/scheme/" + strings.TrimPrefix(id, "builtin:") + ".ini"
	}
	if strings.HasPrefix(id, "http://") || strings.HasPrefix(id, "https://") {
		return id
	}
	return ""
}

// loadScheme resolves a scheme id: a preset id or an ini URL. Remote schemes
// are cached; when a refresh fails the stale copy keeps serving.
func (s *Server) loadScheme(ctx context.Context, id string) (*sub.Scheme, error) {
	if id == "" {
		return nil, nil
	}
	var sc *sub.Scheme
	if p, ok := sub.Preset(id); ok && p.Text != "" {
		var err error
		if sc, err = sub.ParseScheme(p.Text); err != nil {
			return nil, err
		}
	} else {
		src := s.schemeSourceURL(id)
		if src == "" {
			return nil, fmt.Errorf("未知的规则方案 %q", id)
		}
		v, have, fresh := s.schemes.get("scheme:"+src, schemeTTL)
		if !fresh {
			body, err := s.fetchText(ctx, src, 1<<20)
			if err == nil {
				var parsed *sub.Scheme
				if parsed, err = sub.ParseScheme(string(body)); err == nil {
					v = cached{sc: parsed}
					s.schemes.put("scheme:"+src, v)
				}
			}
			if err != nil {
				if !have {
					return nil, fmt.Errorf("规则方案读取失败: %w", err)
				}
				s.log.Warn("rule scheme refresh failed, using cached copy", "url", src, "err", err)
			}
		}
		cp := *v.sc
		sc = &cp
	}
	sc.RuleSetURL = s.ruleSetURL
	return sc, nil
}

func (s *Server) fetchText(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.safeClient(20 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", u.Host, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// ruleSetURL signs a rule list URL so /sub/rs only converts lists that a
// scheme rendered by this panel refers to (it is not an open proxy).
func (s *Server) ruleSetURL(listURL string) string {
	enc := base64.RawURLEncoding.EncodeToString([]byte(listURL))
	return s.subBase() + "/sub/rs/" + s.ruleSetSig(listURL) + "/" + enc + ".json"
}

func (s *Server) ruleSetSig(listURL string) string {
	m := hmac.New(sha256.New, s.jwtKey)
	m.Write([]byte("ruleset:" + listURL))
	return hex.EncodeToString(m.Sum(nil))[:24]
}

// serveRuleSet converts a classic (Clash / Surge) rule list into a sing-box
// source rule set.
func (s *Server) serveRuleSet(c *gin.Context) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSuffix(c.Param("enc"), ".json"))
	listURL := string(raw)
	if err != nil || !hmac.Equal([]byte(c.Param("sig")), []byte(s.ruleSetSig(listURL))) {
		c.String(http.StatusNotFound, "404 page not found")
		return
	}
	v, have, fresh := s.schemes.get("rs:"+listURL, ruleSetTTL)
	if !fresh {
		body, err := s.fetchText(c.Request.Context(), listURL, 8<<20)
		if err == nil {
			v = cached{body: listToRuleSet(body)}
			s.schemes.put("rs:"+listURL, v)
		} else if !have {
			c.String(http.StatusBadGateway, "rule list unavailable")
			return
		}
	}
	c.Header("Cache-Control", "public, max-age=3600")
	c.Data(http.StatusOK, "application/json", v.body)
}

func listToRuleSet(list []byte) []byte {
	fields := map[string]string{"DOMAIN": "domain", "DOMAIN-SUFFIX": "domain_suffix", "DOMAIN-KEYWORD": "domain_keyword",
		"DOMAIN-REGEX": "domain_regex", "IP-CIDR": "ip_cidr", "IP-CIDR6": "ip_cidr", "SRC-IP-CIDR": "source_ip_cidr"}
	match := map[string][]string{} // matched with OR inside one rule
	var procs []string
	var ports []int
	sc := bufio.NewScanner(bytes.NewReader(list))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' || strings.HasPrefix(line, "//") {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		t, v := strings.ToUpper(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])
		switch {
		case fields[t] != "":
			match[fields[t]] = append(match[fields[t]], v)
		case t == "PROCESS-NAME":
			procs = append(procs, v)
		case t == "DST-PORT":
			if p, err := strconv.Atoi(v); err == nil {
				ports = append(ports, p)
			}
		}
	}
	rules := []map[string]any{}
	if len(match) > 0 {
		r := map[string]any{}
		for k, v := range match {
			r[k] = v
		}
		rules = append(rules, r)
	}
	if len(procs) > 0 {
		rules = append(rules, map[string]any{"process_name": procs})
	}
	if len(ports) > 0 {
		rules = append(rules, map[string]any{"port": ports})
	}
	b, _ := json.Marshal(map[string]any{"version": 2, "rules": rules})
	return b
}

// serveSchemeINI publishes the built-in presets as ini for third-party backends.
func (s *Server) serveSchemeINI(c *gin.Context) {
	p, ok := sub.Preset("builtin:" + strings.TrimSuffix(c.Param("name"), ".ini"))
	if !ok || p.Text == "" {
		c.String(http.StatusNotFound, "404 page not found")
		return
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(p.Text))
}

// checkScheme loads a scheme for the UI and summarises it.
func (s *Server) checkScheme(c *gin.Context) {
	id := c.Query("id")
	sc, err := s.loadScheme(c.Request.Context(), id)
	if err != nil {
		badRequest(c, err)
		return
	}
	if sc == nil {
		c.JSON(http.StatusOK, gin.H{"groups": []string{}, "rules": 0, "remote": 0})
		return
	}
	groups := make([]string, 0, len(sc.Groups))
	for _, g := range sc.Groups {
		groups = append(groups, g.Name)
	}
	remote := 0
	for _, r := range sc.Rules {
		if r.URL != "" {
			remote++
		}
	}
	c.JSON(http.StatusOK, gin.H{"groups": groups, "rules": len(sc.Rules), "remote": remote, "source": s.schemeSourceURL(id)})
}

// applyNodeOpts applies the "skip certificate verification" and "force UDP"
// switches to every node.
func applyNodeOpts(nodes []sub.Node, ro renderOpts) []sub.Node {
	if !ro.SkipCert && !ro.ForceUDP {
		return nodes
	}
	out := make([]sub.Node, len(nodes))
	for i, n := range nodes {
		if ro.SkipCert {
			n.TLS.Insecure = true
		}
		n.ForceUDP = ro.ForceUDP
		out[i] = n
	}
	return out
}

// externalParams merges the render switches into the backend query string.
func externalParams(params string, ro renderOpts) string {
	q, err := url.ParseQuery(strings.TrimPrefix(params, "&"))
	if err != nil {
		return params
	}
	if ro.SkipCert {
		q.Set("scv", "true")
	}
	if ro.ForceUDP {
		q.Set("udp", "true")
	}
	return q.Encode()
}
