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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// cfClient is a minimal Cloudflare API v4 client (DNS, SSL mode, Origin CA).
type cfClient struct {
	token string
	base  string
	http  *http.Client
}

func newCF(token string) *cfClient {
	return &cfClient{token: token, base: "https://api.cloudflare.com/client/v4", http: &http.Client{Timeout: 20 * time.Second}}
}

func (c *cfClient) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
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

type cfRecord struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

// records lists the A/AAAA/CNAME records of a name.
func (c *cfClient) records(ctx context.Context, zone, name string) ([]cfRecord, error) {
	var all []cfRecord
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%s/dns_records?name=%s", zone, name), nil, &all); err != nil {
		return nil, err
	}
	var out []cfRecord
	for _, r := range all {
		if r.Type == "A" || r.Type == "AAAA" || r.Type == "CNAME" {
			out = append(out, r)
		}
	}
	return out, nil
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
	// expires_on comes as "2041-10-24 16:58:00 +0000 UTC", not RFC 3339;
	// the expiry is read from the certificate itself instead.
	var res struct {
		Certificate string `json:"certificate"`
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
	NodeCheck     bool     `json:"nodeCheck"`
	StaleHours    int      `json:"staleHours"`
	CarrierSplit  bool     `json:"carrierSplit"`
	APIToken      string   `json:"apiToken"` // empty = unchanged
}

var cdnHostRe = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{2,63}$`)

// normalize cleans what an admin typed and refuses what clients could not
// use: a URL instead of a domain, ports outside 1–65535 or (for Cloudflare)
// ports it does not proxy over HTTPS, malformed preferred addresses.
func (r *cdnReq) normalize() error {
	r.Name = strings.TrimSpace(r.Name)
	d := strings.ToLower(strings.TrimSpace(r.OriginDomain))
	d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
	if i := strings.IndexAny(d, "/:"); i >= 0 {
		d = d[:i]
	}
	d = strings.TrimSuffix(d, ".")
	if !cdnHostRe.MatchString(d) || strings.HasPrefix(d, "*.") {
		return fmt.Errorf("回源域名应为域名，例如 cdn.example.com（填的是 %q）", r.OriginDomain)
	}
	r.OriginDomain = d
	r.Provider = orDefault(r.Provider, "cloudflare")
	if r.Provider != "cloudflare" && r.Provider != "other" {
		return fmt.Errorf("未知的服务商 %q", r.Provider)
	}
	r.TLSMode = orDefault(r.TLSMode, "full_strict")
	if !slices.Contains([]string{"full_strict", "full", "flexible"}, r.TLSMode) {
		return fmt.Errorf("未知的 TLS 模式 %q", r.TLSMode)
	}
	var ports []int
	for _, p := range r.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("端口 %d 不在 1–65535 之间", p)
		}
		if r.Provider == "cloudflare" && !slices.Contains(cloudflareHTTPSPorts, p) {
			return fmt.Errorf("Cloudflare 只在 443、2053、2083、2087、2096、8443 上代理 HTTPS，客户端连不了 %d", p)
		}
		if !slices.Contains(ports, p) {
			ports = append(ports, p)
		}
	}
	r.Ports = ports
	var ips []string
	for _, ip := range r.PreferredIPs {
		ip = strings.Trim(strings.TrimSpace(ip), "[]")
		if ip == "" {
			continue
		}
		if net.ParseIP(ip) == nil && !cdnHostRe.MatchString(strings.ToLower(ip)) {
			return fmt.Errorf("优选地址 %q 不是 IP 或域名（不要带端口）", ip)
		}
		if !slices.Contains(ips, ip) {
			ips = append(ips, ip)
		}
	}
	r.PreferredIPs = ips
	if u := strings.TrimSpace(r.IPSourceURL); u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return fmt.Errorf("优选 IP 来源应为 http(s):// 网址")
	}
	r.IPSourceURL = strings.TrimSpace(r.IPSourceURL)
	r.IPTopN = max(r.IPTopN, 0)
	return nil
}

func (r *cdnReq) apply(p *store.CDNProfile, box func(string) string) {
	p.Name, p.Provider, p.OriginDomain, p.TLSMode = r.Name, orDefault(r.Provider, "cloudflare"), r.OriginDomain, orDefault(r.TLSMode, "full_strict")
	if len(r.Ports) == 0 {
		r.Ports = []int{443, 2053, 2083, 2087, 2096, 8443}
	}
	p.Ports, p.PreferredIPs = store.NewJSON(r.Ports), store.NewJSON(nonNilStrs(r.PreferredIPs))
	p.NodeCheck, p.StaleHours, p.CarrierSplit = r.NodeCheck, min(max(r.StaleHours, 0), 24*30), r.CarrierSplit
	if !p.NodeCheck {
		p.BadIPs = store.NewJSON([]string{})
	}
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
		Inbounds int64               `json:"inbounds"`
		Reports  []store.CDNIPReport `json:"reports"`
		Probes   []cdnProbeView      `json:"probes"`
	}
	ids := make([]string, len(ps))
	for i := range ps {
		ids[i] = ps[i].ID
	}
	inbounds := countDistinctBy(s.st.DB, &store.Endpoint{}, "cdn_profile_id", "inbound_id", ids)
	reports := s.cdnReports(ids)
	probes := s.cdnProbes(ids)
	out := make([]view, 0, len(ps))
	for _, p := range ps {
		p.HasToken = p.APIToken != ""
		pv := probes[p.ID]
		if pv == nil {
			pv = []cdnProbeView{}
		}
		out = append(out, view{CDNProfile: p, Inbounds: inbounds[p.ID], Reports: nonNilReports(reports[p.ID]), Probes: pv})
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) createCDN(c *gin.Context) {
	var req cdnReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := req.normalize(); err != nil {
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
	if err := req.normalize(); err != nil {
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
	// Caddy fronting (and its port) shape the nodes' configuration.
	var inboundIDs []string
	s.st.DB.Model(&store.Endpoint{}).Where("cdn_profile_id = ?", p.ID).Distinct().Pluck("inbound_id", &inboundIDs)
	if len(inboundIDs) > 0 {
		s.MarkDirty("cdn profile "+p.Name+" updated", s.nodesForInbounds(inboundIDs...)...)
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
	s.st.DB.Where("cdn_profile_id = ?", c.Param("id")).Delete(&store.APIToken{})
	s.auditCtx(c, "cdn.delete", c.Param("id"), "")
	c.Status(http.StatusNoContent)
}

// syncCDNDNS creates proxied DNS records for every node behind the profile,
// sets the zone's SSL mode and, under Full (strict), installs a Cloudflare
// Origin certificate on the inbounds that serve only CDN clients.
func (s *Server) syncCDNDNS(c *gin.Context) {
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	res, err := s.syncCDN(c.Request.Context(), &p, true)
	if err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "cdn.sync_dns", p.Name, strings.Join(res.Records, ", "))
	c.JSON(http.StatusOK, res)
}

type cdnSyncResult struct {
	Records []string `json:"records"`
	SSLMode string   `json:"sslMode,omitempty"`
	Cert    string   `json:"cert,omitempty"`
	// Split lists entries moved to their node's own hostname because
	// several nodes shared one.
	Split []string `json:"split,omitempty"`
}

// syncCDN upserts the profile's DNS records; full also sets the zone's SSL
// mode and installs Origin certificates (the "同步 DNS" button). Without
// full it only keeps records pointing at the nodes (自动同步).
func (s *Server) syncCDN(ctx context.Context, p *store.CDNProfile, full bool) (cdnSyncResult, error) {
	res := cdnSyncResult{Records: []string{}}
	token := s.st.Box.MustOpen(p.APIToken)
	if token == "" {
		return res, fmt.Errorf("该配置档未填写 Cloudflare API Token")
	}
	cf := newCF(token)
	zone := p.ZoneID
	var err error
	if zone == "" {
		if zone, err = cf.zoneFor(ctx, p.OriginDomain); err != nil {
			return res, err
		}
	}
	res.Split = s.splitSharedCDNHosts(p)
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
			return res, fmt.Errorf("回源域名 %s 同时指向多个节点（%s / %s），请在入口里为每台节点单独填 Host（如 sg-%s）", host, prev, nodeOrigin(&n), host)
		}
		records[host] = nodeOrigin(&n)
		inboundIDs = append(inboundIDs, in.ID)
	}
	for host, addr := range records {
		if err := cf.upsertRecord(ctx, zone, host, addr); err != nil {
			s.st.DB.Model(p).Update("last_error", truncate(err.Error(), 1900))
			return res, err
		}
		res.Records = append(res.Records, host+" → "+addr)
	}
	s.warnWrite("cdn zone", s.st.DB.Model(p).Updates(map[string]any{"zone_id": zone, "last_error": ""}).Error) // only a cache of the zone lookup
	if !full {
		return res, nil
	}
	mode := map[string]string{"full_strict": "strict", "full": "full", "flexible": "flexible"}[p.TLSMode]
	if mode == "" {
		mode = "strict"
	}
	// Under strict the certificates must be in place first: switching
	// the zone before they are would break every CDN client (526).
	if mode == "strict" && len(records) > 0 {
		note, ok := s.installOriginCert(ctx, cf, records, inboundIDs)
		res.Cert = note
		if !ok {
			res.Cert += "；SSL 模式没有改动"
			return res, nil
		}
	}
	if err := cf.setSSLMode(ctx, zone, mode); err != nil {
		return res, err
	}
	res.SSLMode = mode
	return res, nil
}

// installOriginCert issues a Cloudflare Origin certificate for the hosts
// and puts it on the TLS inbounds. Clients do not trust Origin
// certificates, so inbounds that also serve direct clients keep theirs.
func (s *Server) installOriginCert(ctx context.Context, cf *cfClient, records map[string]string, inboundIDs []string) (string, bool) {
	var targets, kept []string
	for _, id := range inboundIDs {
		var in store.Inbound
		if s.st.DB.First(&in, "id = ?", id).Error != nil || in.Security.V.Type != "tls" || slices.Contains(targets, id) || slices.Contains(kept, in.Tag) {
			continue
		}
		var direct int64
		s.st.DB.Model(&store.Endpoint{}).Where("inbound_id = ? AND kind = ?", id, "direct").Count(&direct)
		if direct > 0 {
			kept = append(kept, in.Tag)
			continue
		}
		targets = append(targets, id)
	}
	keptNote := ""
	if len(kept) > 0 {
		keptNote = "；这些入站还有直连入口，客户端不认 Origin 证书，没有换：" + strings.Join(kept, "、") + "（需要它们证书包含回源域名，或把 SSL 模式改为 Full）"
	}
	if len(targets) == 0 {
		return "没有入站需要换证书" + keptNote, true
	}
	var hosts []string
	for h := range records {
		hosts = append(hosts, h)
	}
	slices.Sort(hosts)
	certPEM, keyPEM, err := cf.originCert(ctx, hosts)
	if err != nil {
		return "Origin 证书签发失败：" + err.Error() + keptNote, false
	}
	exp, _ := pki.ParseCertExpiry([]byte(certPEM))
	cert := store.Certificate{ID: store.NewID(), Domain: hosts[0], Provider: "cf-origin", Cert: certPEM, Key: s.st.Box.Seal(keyPEM), ExpireAt: &exp}
	if err := s.st.DB.Create(&cert).Error; err != nil {
		return "Origin 证书已签发，但保存失败：" + err.Error() + keptNote, false
	}
	var failed []string
	for _, id := range targets {
		var in store.Inbound
		if s.st.DB.First(&in, "id = ?", id).Error == nil {
			in.Security.V.CertificateID = cert.ID
			if err := s.st.DB.Model(&in).Update("security", in.Security).Error; err != nil {
				failed = append(failed, in.Tag)
			}
		}
	}
	s.MarkDirty("cloudflare origin cert", s.nodesForInbounds(targets...)...)
	if len(failed) > 0 {
		return "Origin 证书已签发，但这些入站没能换上新证书，请稍后重试：" + strings.Join(failed, "、") + keptNote, false
	}
	return "Origin 证书已下发" + keptNote, true
}

// autoSyncCDNDNS keeps the DNS records of profiles with 自动同步 on.
func (s *Server) autoSyncCDNDNS() {
	var ps []store.CDNProfile
	s.st.DB.Where("auto_dns = ? AND api_token <> ''", true).Find(&ps)
	for i := range ps {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if _, err := s.syncCDN(ctx, &ps[i], false); err != nil {
			s.st.DB.Model(&ps[i]).Update("last_error", truncate("自动同步 DNS："+err.Error(), 1900))
		}
		cancel()
	}
}

// autoSyncCDNProfile syncs one profile's DNS in the background after an
// entry is added, when the profile asks for it.
func (s *Server) autoSyncCDNProfile(id string) {
	var p store.CDNProfile
	if id == "" || s.st.DB.First(&p, "id = ?", id).Error != nil || !p.AutoDNS || p.APIToken == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := s.syncCDN(ctx, &p, false); err != nil {
			s.st.DB.Model(&p).Update("last_error", truncate("自动同步 DNS："+err.Error(), 1900))
		}
	}()
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
	now := time.Now()
	for i := range ps {
		if s.cdnFreshReport(&ps[i], now) {
			continue // speed-test points maintain this list
		}
		if _, err := s.refreshPreferredIPs(context.Background(), &ps[i]); err != nil {
			s.st.DB.Model(&ps[i]).Update("last_error", truncate(err.Error(), 1900))
		}
	}
}
