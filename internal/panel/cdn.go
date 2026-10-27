package panel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// cfClient is a minimal Cloudflare API v4 client (DNS, SSL mode, Origin CA).
type cfClient struct {
	token string
	http  *http.Client
}

func newCF(token string) *cfClient {
	return &cfClient{token: token, http: &http.Client{Timeout: 20 * time.Second}}
}

func (c *cfClient) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool                       `json:"success"`
		Errors  []struct{ Message string } `json:"errors"`
		Result  json.RawMessage            `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&env); err != nil {
		return fmt.Errorf("cloudflare %s %s: %s", method, path, resp.Status)
	}
	if !env.Success {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("cloudflare: %s", strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// zoneFor finds the zone containing host by walking up its labels.
func (c *cfClient) zoneFor(ctx context.Context, host string) (string, error) {
	parts := strings.Split(strings.TrimSuffix(host, "."), ".")
	for i := 0; i < len(parts)-1; i++ {
		name := strings.Join(parts[i:], ".")
		var zones []struct{ ID string }
		if err := c.do(ctx, http.MethodGet, "/zones?name="+name, nil, &zones); err != nil {
			return "", err
		}
		if len(zones) > 0 {
			return zones[0].ID, nil
		}
	}
	return "", fmt.Errorf("no Cloudflare zone found for %s", host)
}

// upsertRecord creates or updates a proxied A/AAAA record.
func (c *cfClient) upsertRecord(ctx context.Context, zone, name, content string) error {
	return c.setRecord(ctx, zone, name, recordType(content), content, true, 1)
}

func recordType(content string) string {
	if ip := net.ParseIP(content); ip != nil {
		if ip.To4() == nil {
			return "AAAA"
		}
		return "A"
	}
	return "CNAME"
}

// setRecord upserts one record of the given type (ttl 1 = automatic).
func (c *cfClient) setRecord(ctx context.Context, zone, name, typ, content string, proxied bool, ttl int) error {
	var recs []struct{ ID string }
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%s/dns_records?type=%s&name=%s", zone, typ, name), nil, &recs); err != nil {
		return err
	}
	if ttl <= 0 {
		ttl = 1
	}
	body := map[string]any{"type": typ, "name": name, "content": content, "proxied": proxied, "ttl": ttl, "comment": "managed by Polaris"}
	if len(recs) > 0 {
		return c.do(ctx, http.MethodPut, fmt.Sprintf("/zones/%s/dns_records/%s", zone, recs[0].ID), body, nil)
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/zones/%s/dns_records", zone), body, nil)
}

// deleteRecords removes records of a type for a name.
func (c *cfClient) deleteRecords(ctx context.Context, zone, name, typ string) error {
	var recs []struct{ ID string }
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%s/dns_records?type=%s&name=%s", zone, typ, name), nil, &recs); err != nil {
		return err
	}
	for _, r := range recs {
		if err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/zones/%s/dns_records/%s", zone, r.ID), nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *cfClient) setSSLMode(ctx context.Context, zone, mode string) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/zones/%s/settings/ssl", zone), map[string]string{"value": mode}, nil)
}

// originCert requests a 15-year Cloudflare Origin CA certificate.
func (c *cfClient) originCert(ctx context.Context, hosts []string) (certPEM, keyPEM string, err error) {
	key, csr, err := pki.NewCSR(hosts[0])
	if err != nil {
		return "", "", err
	}
	var res struct {
		Certificate string    `json:"certificate"`
		ExpiresOn   time.Time `json:"expires_on"`
	}
	err = c.do(ctx, http.MethodPost, "/certificates", map[string]any{
		"hostnames": hosts, "requested_validity": 5475, "request_type": "origin-ecc", "csr": string(csr),
	}, &res)
	if err != nil {
		return "", "", err
	}
	return res.Certificate, string(key), nil
}

// --- API ---

type cdnReq struct {
	Name          string   `json:"name" binding:"required"`
	Provider      string   `json:"provider"`
	OriginDomain  string   `json:"originDomain" binding:"required"`
	TLSMode       string   `json:"tlsMode"`
	Ports         []int    `json:"ports"`
	PreferredIPs  []string `json:"preferredIps"`
	IPSourceURL   string   `json:"ipSourceUrl"`
	IPTopN        int      `json:"ipTopN"`
	ExpandPerIP   bool     `json:"expandPerIp"`
	AutoDNS       bool     `json:"autoDns"`
	CaddyFronting bool     `json:"caddyFronting"`
	APIToken      string   `json:"apiToken"` // empty = unchanged
}

func (r *cdnReq) apply(p *store.CDNProfile, box func(string) string) {
	p.Name, p.Provider, p.OriginDomain, p.TLSMode = r.Name, orDefault(r.Provider, "cloudflare"), r.OriginDomain, orDefault(r.TLSMode, "full_strict")
	if len(r.Ports) == 0 {
		r.Ports = []int{443, 2053, 2083, 2087, 2096, 8443}
	}
	p.Ports, p.PreferredIPs = store.NewJSON(r.Ports), store.NewJSON(nonNilStrs(r.PreferredIPs))
	p.IPSourceURL, p.IPTopN, p.ExpandPerIP, p.AutoDNS, p.CaddyFronting = r.IPSourceURL, r.IPTopN, r.ExpandPerIP, r.AutoDNS, r.CaddyFronting
	if r.APIToken != "" {
		p.APIToken = box(r.APIToken)
	}
}

func (s *Server) listCDN(c *gin.Context) {
	var ps []store.CDNProfile
	s.st.DB.Order("name").Find(&ps)
	type view struct {
		store.CDNProfile
		Inbounds int64 `json:"inbounds"`
	}
	ids := make([]string, len(ps))
	for i := range ps {
		ids[i] = ps[i].ID
	}
	inbounds := countDistinctBy(s.st.DB, &store.Endpoint{}, "cdn_profile_id", "inbound_id", ids)
	out := make([]view, 0, len(ps))
	for _, p := range ps {
		p.HasToken = p.APIToken != ""
		out = append(out, view{CDNProfile: p, Inbounds: inbounds[p.ID]})
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) createCDN(c *gin.Context) {
	var req cdnReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	p := store.CDNProfile{ID: store.NewID()}
	req.apply(&p, s.st.Box.Seal)
	if err := s.st.DB.Create(&p).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "cdn.create", p.Name, p.OriginDomain)
	c.JSON(http.StatusCreated, p)
}

func (s *Server) updateCDN(c *gin.Context) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req cdnReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.CaddyFronting {
		var ins []store.Inbound
		if err := s.st.DB.Model(&store.Inbound{}).Joins("JOIN endpoints ep ON ep.inbound_id = inbounds.id").Where("ep.cdn_profile_id = ?", p.ID).Find(&ins).Error; err != nil {
			dbError(c, err)
			return
		}
		for _, in := range ins {
			if in.RelayAccess.V.Enabled {
				badRequest(c, fmt.Errorf("关联入站已开启来源限制，不能启用 Caddy 共用回源"))
				return
			}
		}
	}
	req.apply(&p, s.st.Box.Seal)
	if err := s.st.DB.Save(&p).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "cdn.update", p.Name, "")
	c.JSON(http.StatusOK, p)
}

func (s *Server) deleteCDN(c *gin.Context) {
	var n int64
	s.st.DB.Model(&store.Endpoint{}).Where("cdn_profile_id = ?", c.Param("id")).Count(&n)
	if n > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("仍有 %d 个入口使用该配置档", n)})
		return
	}
	s.st.DB.Delete(&store.CDNProfile{}, "id = ?", c.Param("id"))
	s.auditCtx(c, "cdn.delete", c.Param("id"), "")
	c.Status(http.StatusNoContent)
}

// syncCDNDNS creates proxied DNS records for every node behind the profile,
// sets SSL to Full (strict) and installs a Cloudflare Origin certificate on
// the inbounds that serve the origin domain.
func (s *Server) syncCDNDNS(c *gin.Context) {
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	token := s.st.Box.MustOpen(p.APIToken)
	if token == "" {
		badRequest(c, fmt.Errorf("该配置档未填写 Cloudflare API Token"))
		return
	}
	cf := newCF(token)
	ctx := c.Request.Context()
	zone := p.ZoneID
	var err error
	if zone == "" {
		if zone, err = cf.zoneFor(ctx, p.OriginDomain); err != nil {
			badRequest(c, err)
			return
		}
	}
	var eps []store.Endpoint
	s.st.DB.Where("cdn_profile_id = ?", p.ID).Find(&eps)
	records := map[string]string{} // hostname -> origin address
	var inboundIDs []string
	for _, ep := range eps {
		var in store.Inbound
		if s.st.DB.First(&in, "id = ?", ep.InboundID).Error != nil {
			continue
		}
		var n store.Node
		if s.st.DB.First(&n, "id = ?", in.NodeID).Error != nil {
			continue
		}
		host := orDefault(ep.Host, p.OriginDomain)
		if prev, ok := records[host]; ok && prev != nodeOrigin(&n) {
			badRequest(c, fmt.Errorf("回源域名 %s 同时指向多个节点（%s / %s），请为入口单独设置 Host", host, prev, nodeOrigin(&n)))
			return
		}
		records[host] = nodeOrigin(&n)
		inboundIDs = append(inboundIDs, in.ID)
	}
	var done []string
	for host, addr := range records {
		if err := cf.upsertRecord(ctx, zone, host, addr); err != nil {
			s.st.DB.Model(&p).Update("last_error", truncate(err.Error(), 1900))
			badRequest(c, err)
			return
		}
		done = append(done, host+" → "+addr)
	}
	mode := map[string]string{"full_strict": "strict", "full": "full", "flexible": "flexible"}[p.TLSMode]
	if mode == "" {
		mode = "strict"
	}
	if err := cf.setSSLMode(ctx, zone, mode); err != nil {
		badRequest(c, err)
		return
	}
	certNote := ""
	if mode == "strict" && len(records) > 0 {
		var hosts []string
		for h := range records {
			hosts = append(hosts, h)
		}
		certPEM, keyPEM, err := cf.originCert(ctx, hosts)
		if err != nil {
			certNote = "Origin 证书签发失败：" + err.Error()
		} else {
			exp, _ := pki.ParseCertExpiry([]byte(certPEM))
			cert := store.Certificate{ID: store.NewID(), Domain: hosts[0], Provider: "cf-origin", Cert: certPEM,
				Key: s.st.Box.Seal(keyPEM), ExpireAt: &exp}
			if err := s.st.DB.Create(&cert).Error; err != nil {
				certNote = "Origin 证书已签发，但保存失败：" + err.Error()
			} else {
				var failed []string
				for _, id := range inboundIDs {
					var in store.Inbound
					if s.st.DB.First(&in, "id = ?", id).Error == nil && in.Security.V.Type == "tls" {
						in.Security.V.CertificateID = cert.ID
						if err := s.st.DB.Model(&in).Update("security", in.Security).Error; err != nil {
							failed = append(failed, in.Tag)
						}
					}
				}
				certNote = "Origin 证书已下发"
				if len(failed) > 0 {
					certNote = "Origin 证书已签发，但这些入站没能换上新证书，请稍后重试：" + strings.Join(failed, "、")
				}
				s.MarkDirty("cloudflare origin cert", s.nodesForInbounds(inboundIDs...)...)
			}
		}
	}
	s.warnWrite("cdn zone", s.st.DB.Model(&p).Updates(map[string]any{"zone_id": zone, "last_error": ""}).Error) // only a cache of the zone lookup
	s.auditCtx(c, "cdn.sync_dns", p.Name, strings.Join(done, ", "))
	c.JSON(http.StatusOK, gin.H{"records": done, "sslMode": mode, "cert": certNote})
}

func (s *Server) refreshCDNIPs(c *gin.Context) {
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	ips, err := s.refreshPreferredIPs(c.Request.Context(), &p)
	if err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "cdn.refresh_ips", p.Name, fmt.Sprintf("%d IPs", len(ips)))
	c.JSON(http.StatusOK, gin.H{"ips": ips})
}

// refreshPreferredIPs pulls a list of IPs (one per line; ports, CSV columns and
// comments are tolerated) from the profile's source URL.
func (s *Server) refreshPreferredIPs(ctx context.Context, p *store.CDNProfile) ([]string, error) {
	if p.IPSourceURL == "" {
		return nil, fmt.Errorf("未配置优选 IP 来源 URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.IPSourceURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.safeClient(20 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", p.IPSourceURL, resp.Status)
	}
	ips := parseIPList(io.LimitReader(resp.Body, 1<<20))
	if len(ips) == 0 {
		return nil, fmt.Errorf("来源中没有可用的 IP")
	}
	now := time.Now()
	if err := s.st.DB.Model(p).Updates(map[string]any{"preferred_ips": store.NewJSON(ips), "last_refresh_at": now, "last_error": ""}).Error; err != nil {
		return nil, err
	}
	return ips, nil
}

func parseIPList(r io.Reader) []string {
	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexAny(line, "#"); i >= 0 {
			line = line[:i]
		}
		for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == ';' }) {
			host := f
			if h, _, err := net.SplitHostPort(f); err == nil {
				host = h
			}
			host = strings.Trim(host, "[]")
			if ip := net.ParseIP(host); ip != nil && !seen[ip.String()] {
				seen[ip.String()] = true
				out = append(out, ip.String())
				break // first IP column of the line
			}
		}
	}
	return out
}

func (s *Server) refreshAllPreferredIPs() {
	var ps []store.CDNProfile
	s.st.DB.Where("ip_source_url <> ''").Find(&ps)
	for i := range ps {
		if _, err := s.refreshPreferredIPs(context.Background(), &ps[i]); err != nil {
			s.st.DB.Model(&ps[i]).Update("last_error", truncate(err.Error(), 1900))
		}
	}
}
