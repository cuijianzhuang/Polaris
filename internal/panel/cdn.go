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
	"gorm.io/gorm"

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

func (c *cfClient) sslMode(ctx context.Context, zone string) (string, error) {
	var res struct {
		Value string `json:"value"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%s/settings/ssl", zone), nil, &res)
	return res.Value, err
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
	Scheme        string    `json:"scheme"`
	Name          string    `json:"name" binding:"required"`
	Provider      string    `json:"provider"`
	OriginDomain  string    `json:"originDomain" binding:"required"`
	TLSMode       string    `json:"tlsMode"`
	Ports         []int     `json:"ports"`
	PreferredIPs  []string  `json:"preferredIps"`
	PinnedHosts   *[]string `json:"pinnedHosts"` // nil keeps the legacy mixed-list contract
	IPSourceURL   string    `json:"ipSourceUrl"`
	IPTopN        int       `json:"ipTopN"`
	ExpandPerIP   bool      `json:"expandPerIp"`
	AutoDNS       bool      `json:"autoDns"`
	CaddyFronting bool      `json:"caddyFronting"`
	NodeCheck     bool      `json:"nodeCheck"`
	StaleHours    int       `json:"staleHours"`
	CarrierSplit  bool      `json:"carrierSplit"`
	APIToken      string    `json:"apiToken"` // empty = unchanged
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
	r.Scheme = orDefault(r.Scheme, "https")
	if r.Scheme != "http" && r.Scheme != "https" {
		return fmt.Errorf("CDN 连接方式只能是 HTTP 或 HTTPS")
	}
	if r.Scheme == "http" && r.CaddyFronting {
		return fmt.Errorf("HTTP CDN 不支持内置 Caddy 统一回源，请关闭该选项或使用 HTTPS")
	}
	profile := &store.CDNProfile{Scheme: r.Scheme}
	var ports []int
	for _, p := range r.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("端口 %d 不在 1–65535 之间", p)
		}
		if r.Provider == "cloudflare" && !slices.Contains(cdnClientPorts(profile), p) {
			return fmt.Errorf("Cloudflare 的 %s 端口为 %s，客户端连不了 %d", strings.ToUpper(r.Scheme), cdnPortsText(profile), p)
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
	if r.PinnedHosts != nil {
		hosts, err := normalizeCDNPinnedHosts(*r.PinnedHosts)
		if err != nil {
			return err
		}
		r.PinnedHosts = &hosts
	}
	if u := strings.TrimSpace(r.IPSourceURL); u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return fmt.Errorf("优选 IP 来源应为 http(s):// 网址")
	}
	r.IPSourceURL = strings.TrimSpace(r.IPSourceURL)
	r.IPTopN = max(r.IPTopN, 0)
	return nil
}

func (r *cdnReq) apply(p *store.CDNProfile, box func(string) string) {
	p.Scheme = orDefault(r.Scheme, "https")
	p.Name, p.Provider, p.OriginDomain, p.TLSMode = r.Name, orDefault(r.Provider, "cloudflare"), r.OriginDomain, orDefault(r.TLSMode, "full_strict")
	if len(r.Ports) == 0 {
		r.Ports = slices.Clone(cdnClientPorts(&store.CDNProfile{Scheme: r.Scheme}))
	}
	// A domain is typed in (kept across refreshes) when it already was, or
	// is new in the form: domains a source URL put in the list come back
	// with the form unchanged and must not become permanent by a save.
	oldList, oldPinned := p.PreferredIPs.V, p.PinnedHosts.V
	p.Ports, p.PreferredIPs = store.NewJSON(r.Ports), store.NewJSON(nonNilStrs(r.PreferredIPs))
	pinned := []string{}
	for _, a := range r.PreferredIPs {
		if net.ParseIP(a) == nil && (slices.Contains(oldPinned, a) || !slices.Contains(oldList, a)) {
			pinned = append(pinned, a)
		}
	}
	p.PinnedHosts = store.NewJSON(pinned)
	if r.PinnedHosts != nil {
		setCDNPinnedHosts(p, *r.PinnedHosts, oldPinned)
	}
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
		Serves   []cdnServe          `json:"serves"`
		Reports  []store.CDNIPReport `json:"reports"`
		// Shared: speed-test points of other Cloudflare profiles whose
		// fresh reports this profile's list is also built from.
		Shared []string `json:"shared"`
		// ThirdParty: preferred addresses outside Cloudflare's ranges
		// (reverse proxies from the Shodan source, or typed in).
		ThirdParty []string       `json:"thirdParty"`
		Probes     []cdnProbeView `json:"probes"`
	}
	ids := make([]string, len(ps))
	for i := range ps {
		ids[i] = ps[i].ID
	}
	inbounds := countDistinctBy(s.st.DB, &store.Endpoint{}, "cdn_profile_id", "inbound_id", ids)
	serves := s.cdnServes(ids)
	reports := s.cdnReports(ids)
	probes := s.cdnProbes(ids)
	// Read once: list queries stay flat.
	cdnTok, dnsTok := s.st.GetSetting("cdn_cf_token", "") != "", s.st.GetSetting("acme_cf_token", "") != ""
	out := make([]view, 0, len(ps))
	for _, p := range ps {
		p.TokenSource = tokenSource(p.APIToken != "", cdnTok, dnsTok)
		p.HasToken = p.TokenSource != ""
		pv := probes[p.ID]
		if pv == nil {
			pv = []cdnProbeView{}
		}
		sv := serves[p.ID]
		for i := range sv {
			sv[i].Host = orDefault(sv[i].Host, p.OriginDomain)
		}
		if sv == nil {
			sv = []cdnServe{}
		}
		shared := []string{}
		if p.Provider == "cloudflare" {
			since := time.Now().Add(-cdnStaleAfter(&p))
			for _, o := range ps {
				if o.ID == p.ID || o.Provider != "cloudflare" || cdnScheme(&o) != cdnScheme(&p) {
					continue
				}
				for _, r := range reports[o.ID] {
					own := slices.ContainsFunc(reports[p.ID], func(x store.CDNIPReport) bool { return x.Source == r.Source })
					if r.At.After(since) && !own && !slices.Contains(shared, r.Source) {
						shared = append(shared, r.Source)
					}
				}
			}
		}
		out = append(out, view{CDNProfile: p, Inbounds: inbounds[p.ID], Serves: sv, Shared: shared, ThirdParty: thirdPartyIPs(p.PreferredIPs.V), Reports: nonNilReports(reports[p.ID]), Probes: pv})
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
	if req.Scheme == "" {
		req.Scheme = cdnScheme(&p)
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
	if err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		if err := tx.First(&p, "id = ?", p.ID).Error; err != nil {
			return err
		}
		candidate := p
		req.apply(&candidate, s.st.Box.Seal)
		if err := validateCDNSchemeChange(tx, &p, &candidate); err != nil {
			return err
		}
		p = candidate
		return tx.Save(&p).Error
	}); err != nil {
		if _, ok := err.(cdnSchemeProblem); ok {
			badRequest(c, err)
		} else {
			dbError(c, err)
		}
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

// cdnServe is one CDN entry of a profile: the hostname clients use and
// the node and inbound it leads to.
type cdnServe struct {
	ProfileID string `json:"-"`
	Node      string `json:"node"`
	Tag       string `json:"tag"`
	Host      string `json:"host"`
}

// cdnServes lists the CDN entries of the profiles in one query.
func (s *Server) cdnServes(ids []string) map[string][]cdnServe {
	out := map[string][]cdnServe{}
	if len(ids) == 0 {
		return out
	}
	var rows []cdnServe
	s.st.DB.Table("endpoints").Select("endpoints.cdn_profile_id AS profile_id, nodes.name AS node, inbounds.tag AS tag, endpoints.host AS host").
		Joins("JOIN inbounds ON inbounds.id = endpoints.inbound_id").Joins("JOIN nodes ON nodes.id = inbounds.node_id").
		Where("endpoints.kind = ? AND endpoints.cdn_profile_id IN ?", "cdn", ids).Order("nodes.name, inbounds.tag").Scan(&rows)
	for _, r := range rows {
		out[r.ProfileID] = append(out[r.ProfileID], r)
	}
	return out
}

type cdnUse struct {
	InboundID string `json:"inboundId"`
	Tag       string `json:"tag"`
	Node      string `json:"node"`
	// CDNOnly: the inbound has no other entry, so without this profile it
	// would fall back to a direct entry (the node's address).
	CDNOnly bool `json:"cdnOnly"`
}

// cdnUsage lists the inbounds whose entries use the profile.
func (s *Server) cdnUsage(id string) []cdnUse {
	var inboundIDs []string
	s.st.DB.Model(&store.Endpoint{}).Where("cdn_profile_id = ?", id).Distinct().Pluck("inbound_id", &inboundIDs)
	out := []cdnUse{}
	for _, iid := range inboundIDs {
		var in store.Inbound
		if s.st.DB.Select("id", "tag", "node_id").First(&in, "id = ?", iid).Error != nil {
			continue
		}
		var node store.Node
		s.st.DB.Select("name").First(&node, "id = ?", in.NodeID)
		var others int64
		s.st.DB.Model(&store.Endpoint{}).Where("inbound_id = ? AND (cdn_profile_id <> ? OR kind <> ?)", iid, id, "cdn").Count(&others)
		out = append(out, cdnUse{InboundID: iid, Tag: in.Tag, Node: node.Name, CDNOnly: others == 0})
	}
	return out
}

// deleteCDN is DELETE /cdn-profiles/:id[?force=1]. A profile in use is
// refused with the list of inbounds using it; force also removes their
// entries through it and disables the inbounds left without any entry, so
// they do not fall back to publishing the node's address.
func (s *Server) deleteCDN(c *gin.Context) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	usage := s.cdnUsage(p.ID)
	if len(usage) > 0 && c.Query("force") != "1" {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("仍有 %d 个入站使用该配置档", len(usage)), "usage": usage})
		return
	}
	var disabled, inboundIDs []string
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		for _, u := range usage {
			inboundIDs = append(inboundIDs, u.InboundID)
			if u.CDNOnly {
				if err := tx.Model(&store.Inbound{}).Where("id = ?", u.InboundID).Update("enabled", false).Error; err != nil {
					return err
				}
				disabled = append(disabled, u.Tag)
			}
		}
		if err := tx.Where("cdn_profile_id = ? AND kind = ?", p.ID, "cdn").Delete(&store.Endpoint{}).Error; err != nil {
			return err
		}
		if err := tx.Where("cdn_profile_id = ?", p.ID).Delete(&store.APIToken{}).Error; err != nil {
			return err
		}
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		var gone []store.CDNIPReport
		if err := tx.Where("profile_id = ?", p.ID).Find(&gone).Error; err != nil {
			return err
		}
		if err := tx.Where("profile_id = ?", p.ID).Delete(&store.CDNIPReport{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&store.CDNProfile{}, "id = ?", p.ID).Error; err != nil {
			return err
		}
		// Its points also fed the other Cloudflare profiles.
		if len(gone) == 0 {
			return nil
		}
		_, _, err := rebuildCDNLists(tx, &p, time.Now(), reportIPs(gone))
		return err
	})
	if err != nil {
		serverError(c, err)
		return
	}
	if len(inboundIDs) > 0 {
		s.MarkDirty("cdn profile "+p.Name+" deleted", s.nodesForInbounds(inboundIDs...)...)
	}
	s.auditCtx(c, "cdn.delete", p.Name, fmt.Sprintf("%d inbounds, disabled: %s", len(usage), strings.Join(disabled, ",")))
	if len(usage) == 0 {
		c.Status(http.StatusNoContent)
		return
	}
	c.JSON(http.StatusOK, gin.H{"removed": len(usage), "disabled": disabled})
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
	token := s.cfToken(p)
	if token == "" {
		return res, fmt.Errorf("没有 Cloudflare Token：在「CDN 配置档」页填写 CDN Token（所有配置档共用）")
	}
	cf := newCF(token)
	zone := p.ZoneID
	var err error
	if zone == "" {
		if zone, err = cf.zoneFor(ctx, p.OriginDomain); err != nil {
			return res, cfStep("查找 "+p.OriginDomain+" 所在的域名", "Zone.Zone 读取", err)
		}
	}
	res.Split = s.splitSharedCDNHosts(p)
	var eps []store.Endpoint
	s.st.DB.Where("cdn_profile_id = ?", p.ID).Find(&eps)
	if len(eps) == 0 {
		// Nothing to point at a node; the SSL mode is zone-wide, so it is
		// not touched for a profile no inbound uses.
		return res, fmt.Errorf("还没有入站使用配置档 %s：先在「入站」里给入站添加这个配置档的 CDN 入口，再同步 DNS", p.Name)
	}
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
			err = cfStep("写 DNS 记录 "+host, "Zone.DNS 编辑", err)
			s.st.DB.Model(p).Update("last_error", truncate(err.Error(), 1900))
			return res, err
		}
		res.Records = append(res.Records, host+" → "+addr)
	}
	s.warnWrite("cdn zone", s.st.DB.Model(p).Updates(map[string]any{"zone_id": zone, "last_error": ""}).Error) // only a cache of the zone lookup
	if !full || cdnScheme(p) == "http" {
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
		return res, cfStep("设置 SSL 模式", "Zone.Zone Settings 编辑", err)
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
		return cfStep("签发 Origin 证书", "Zone.SSL and Certificates 编辑", err).Error() + keptNote, false
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

// cfStep names the Cloudflare step that failed and, when the token was
// refused, the permission it needs.
func cfStep(step, perm string, err error) error {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unauthorized") || strings.Contains(msg, "authentication error") || strings.Contains(msg, "403") {
		return fmt.Errorf("%s失败：Cloudflare Token 没有权限，需要「%s」：在「CDN 配置档」页填一个带这项权限的 CDN Token（%v）", step, perm, err)
	}
	return fmt.Errorf("%s失败：%w", step, err)
}

// Cloudflare tokens by what they may do:
//   - DNS token (系统设置, acme_cf_token): Zone read + DNS edit, for ACME DNS
//     validation and Cloudflare DNS providers without a token of their own.
//   - CDN token (CDN 配置档 page, cdn_cf_token): also Zone Settings and SSL
//     and Certificates edit, which 同步 DNS needs; shared by all profiles.
//
// cfToken is the token for a profile: its own (set by older versions),
// else the CDN token, else the DNS token (enough for DNS records only).
func (s *Server) cfToken(p *store.CDNProfile) string {
	for _, sealed := range []string{p.APIToken, s.st.GetSetting("cdn_cf_token", ""), s.st.GetSetting("acme_cf_token", "")} {
		if t := s.st.Box.MustOpen(sealed); t != "" {
			return t
		}
	}
	return ""
}

func tokenSource(own, cdn, dns bool) string {
	switch {
	case own:
		return "profile"
	case cdn:
		return "cdn"
	case dns:
		return "dns"
	}
	return ""
}

// autoSyncCDNDNS keeps the DNS records of profiles with 自动同步 on.
func (s *Server) autoSyncCDNDNS() {
	var ps []store.CDNProfile
	s.st.DB.Where("auto_dns = ?", true).Find(&ps)
	for i := range ps {
		if s.cfToken(&ps[i]) == "" {
			continue
		}
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
	if id == "" || s.st.DB.First(&p, "id = ?", id).Error != nil || !p.AutoDNS || s.cfToken(&p) == "" {
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
		return nil, fmt.Errorf("来源中没有可用的 IP 或域名")
	}
	// The HTTP fetch can overlap a manual edit. Read the current pins only
	// after acquiring the same transaction lock used by report rebuilds.
	if err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		var current store.CDNProfile
		if err := tx.First(&current, "id = ?", p.ID).Error; err != nil {
			return err
		}
		ips = withPinned(ips, current.PinnedHosts.V)
		return tx.Model(&current).Updates(map[string]any{"preferred_ips": store.NewJSON(ips), "last_refresh_at": time.Now(), "last_error": ""}).Error
	}); err != nil {
		return nil, err
	}
	return ips, nil
}

// withPinned is an automatic list of preferred addresses plus the domains
// the admin typed in, which automatic updates never drop.
func withPinned(list, pinned []string) []string {
	out := slices.Clone(list)
	for _, h := range pinned {
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// parseIPList reads preferred addresses: IPs or domains, one per line, the
// first one on each line ("ip:port", CSV columns and # comments allowed).
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
			if ip := net.ParseIP(host); ip != nil {
				if !seen[ip.String()] {
					seen[ip.String()] = true
					out = append(out, ip.String())
				}
				break // first address column of the line
			}
			if h := strings.ToLower(strings.TrimSuffix(host, ".")); cdnHostRe.MatchString(h) && !strings.HasPrefix(h, "*.") {
				if !seen[h] {
					seen[h] = true
					out = append(out, h)
				}
				break
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
