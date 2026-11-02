package panel

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

func cdnTunnel(p *store.CDNProfile) bool { return p.OriginMode == "tunnel" }

var cdnTunnelID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func normalizeCDNTunnel(r *cdnReq) error {
	mode := "direct"
	if r.OriginMode != nil {
		mode = strings.TrimSpace(*r.OriginMode)
		if mode == "" {
			mode = "direct"
		}
		r.OriginMode = &mode
	}
	if mode != "direct" && mode != "tunnel" {
		return fmt.Errorf("回源方式只能为 direct 或 tunnel")
	}
	if mode != "tunnel" {
		return nil
	}
	id := ""
	if r.TunnelID != nil {
		id = strings.ToLower(strings.TrimSpace(*r.TunnelID))
	}
	if !cdnTunnelID.MatchString(id) {
		return fmt.Errorf("请填写 Cloudflare Tunnel UUID，不是 Token 或 URL")
	}
	r.TunnelID = &id
	if r.Provider != "cloudflare" || r.Scheme != "https" || r.CaddyFronting {
		return fmt.Errorf("Tunnel 模式需要 Cloudflare、HTTPS，并关闭 Caddy 统一回源")
	}
	if len(r.Ports) == 0 {
		r.Ports = []int{443}
	}
	if len(r.Ports) != 1 || r.Ports[0] != 443 {
		return fmt.Errorf("Tunnel 模式当前只支持客户端 HTTPS 443 端口")
	}
	return nil
}

func cdnTunnelProblem(m *model.Inbound, p *store.CDNProfile, port int) string {
	if p.Provider != "cloudflare" || cdnScheme(p) != "https" || p.CaddyFronting || !cdnTunnelID.MatchString(p.TunnelID) {
		return "Tunnel 配置无效：需要 Cloudflare、HTTPS 和有效 Tunnel UUID，且不能启用 Caddy"
	}
	if port != 0 && port != 443 {
		return "Tunnel 模式当前只支持客户端 HTTPS 443 端口"
	}
	if len(p.Ports.V) > 0 && (len(p.Ports.V) != 1 || p.Ports.V[0] != 443) {
		return "Tunnel 配置档客户端端口必须为 443"
	}
	if len(m.Extra) > 0 && string(m.Extra) != "null" && string(m.Extra) != "{}" {
		return "Tunnel 模式暂不支持 Extra 覆盖配置，避免覆盖回环监听或 TLS 设置"
	}
	if m.Transport.Type != model.TransWS || m.Security.Type != model.SecNone {
		return "Tunnel 模式当前支持 WS 明文回源：请使用 WS 并关闭入站 TLS；客户端仍使用 HTTPS"
	}
	if m.Listen != "127.0.0.1" {
		return "Tunnel 入站必须监听 127.0.0.1，让同机 cloudflared 回源，避免公开入站端口"
	}
	if m.Transport.AcceptProxyProtocol {
		return "Tunnel HTTP 回源不支持 PROXY protocol，请关闭该选项"
	}
	return ""
}

// One named tunnel belongs to one profile and one node. Sharing its UUID
// between unrelated connectors would load-balance requests to the wrong VPS.
func validateCDNTunnel(db *gorm.DB, p *store.CDNProfile) error {
	if !cdnTunnel(p) {
		return nil
	}
	var count int64
	if err := db.Model(&store.CDNProfile{}).Where("id <> ? AND origin_mode = ? AND tunnel_id = ?", p.ID, "tunnel", p.TunnelID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return cdnSchemeProblem("该 Tunnel UUID 已被其他配置档使用，请为不同节点创建独立隧道")
	}
	var es []store.Endpoint
	if err := db.Where("cdn_profile_id = ?", p.ID).Find(&es).Error; err != nil {
		return err
	}
	node := ""
	for _, ep := range es {
		var row store.Inbound
		if err := db.First(&row, "id = ?", ep.InboundID).Error; err != nil {
			return err
		}
		m := model.Inbound{NodeID: row.NodeID, Listen: row.Listen, Port: row.Port, Transport: row.Transport.V, Security: row.Security.V, Extra: row.Extra}
		if problem := cdnTunnelProblem(&m, p, ep.Port); problem != "" {
			return cdnSchemeProblem(row.Tag + ": " + problem)
		}
		if node != "" && node != row.NodeID {
			return cdnSchemeProblem("一个 Tunnel 配置档只能关联同一台节点")
		}
		node = row.NodeID
		if err := checkTunnelAttachments(db, p, row.ID, row.NodeID); err != nil {
			return cdnSchemeProblem(err.Error())
		}
	}
	return nil
}

func checkTunnelAttachments(db *gorm.DB, p *store.CDNProfile, inboundID, nodeID string) error {
	var others int64
	if err := db.Model(&store.Endpoint{}).Joins("JOIN inbounds ON inbounds.id = endpoints.inbound_id").Where("endpoints.cdn_profile_id = ? AND inbounds.node_id <> ?", p.ID, nodeID).Count(&others).Error; err != nil {
		return err
	}
	if others > 0 {
		return fmt.Errorf("一个 Tunnel 配置档只能关联同一台节点，请为该节点创建独立隧道")
	}
	if err := db.Model(&store.Endpoint{}).Where("inbound_id = ? AND (kind <> ? OR cdn_profile_id <> ?)", inboundID, "cdn", p.ID).Count(&others).Error; err != nil {
		return err
	}
	if others > 0 {
		return fmt.Errorf("Tunnel 入站只能使用此隧道的 CDN 入口，请先移除直连、中转或其他 CDN 入口")
	}
	return nil
}

// Run under lockCDNReports in the same transaction as publication writes.
func validateAttachedTunnels(tx *gorm.DB, inboundID string) error {
	var profiles []store.CDNProfile
	if err := tx.Model(&store.CDNProfile{}).Select("cdn_profiles.*").Joins("JOIN endpoints ON endpoints.cdn_profile_id = cdn_profiles.id").Where("endpoints.inbound_id = ? AND cdn_profiles.origin_mode = ?", inboundID, "tunnel").Find(&profiles).Error; err != nil {
		return err
	}
	for _, p := range profiles {
		if err := validateCDNTunnel(tx, &p); err != nil {
			return err
		}
	}
	return nil
}

// Revalidate persisted state as well: imports and template changes must not
// turn a trusted loopback listener into a public, header-trusting listener.
func (s *Server) inboundTunnel(m *model.Inbound, id string) (bool, error) {
	var profiles []store.CDNProfile
	err := s.st.DB.Model(&store.CDNProfile{}).Select("cdn_profiles.*").Joins("JOIN endpoints ON endpoints.cdn_profile_id = cdn_profiles.id").Where("endpoints.inbound_id = ? AND cdn_profiles.origin_mode = ?", id, "tunnel").Find(&profiles).Error
	if err != nil {
		return false, err
	}
	for _, p := range profiles {
		if problem := cdnTunnelProblem(m, &p, 0); problem != "" {
			return false, fmt.Errorf("%s: %s", m.Tag, problem)
		}
		if err := checkTunnelAttachments(s.st.DB, &p, id, m.NodeID); err != nil {
			return false, err
		}
	}
	return len(profiles) > 0, nil
}

type cdnTunnelRoute struct {
	Hostname      string           `json:"hostname,omitempty" yaml:"hostname,omitempty"`
	Path          string           `json:"path,omitempty" yaml:"path,omitempty"`
	Service       string           `json:"service" yaml:"service"`
	OriginRequest *cdnTunnelOrigin `json:"originRequest,omitempty" yaml:"originRequest,omitempty"`
}
type cdnTunnelOrigin struct {
	HTTPHostHeader string `json:"httpHostHeader" yaml:"httpHostHeader"`
}

func (s *Server) tunnelRoutes(p *store.CDNProfile) ([]cdnTunnelRoute, string, error) {
	if err := validateCDNTunnel(s.st.DB, p); err != nil {
		return nil, "", err
	}
	var entries []store.Endpoint
	if err := s.st.DB.Where("cdn_profile_id = ? AND kind = ?", p.ID, "cdn").Order("id").Find(&entries).Error; err != nil {
		return nil, "", err
	}
	routes := []cdnTunnelRoute{}
	seen := map[string]string{}
	node := ""
	for _, ep := range entries {
		var in store.Inbound
		if err := s.st.DB.First(&in, "id = ?", ep.InboundID).Error; err != nil {
			return nil, "", err
		}
		if !in.Enabled {
			continue
		}
		node = in.NodeID
		host := orDefault(ep.Host, p.OriginDomain)
		if !cdnHostRe.MatchString(host) || strings.HasPrefix(host, "*.") {
			return nil, "", fmt.Errorf("Tunnel Host 必须是有效域名")
		}
		path := orDefault(ep.Path, orDefault(in.Transport.V.Path, "/"))
		if ep.Path != "" && ep.Path != orDefault(in.Transport.V.Path, "/") {
			return nil, "", fmt.Errorf("Tunnel 入口路径必须与入站 WS 路径一致")
		}
		if ep.SNI != "" && ep.SNI != host {
			return nil, "", fmt.Errorf("Tunnel SNI 必须与入口 Host 一致")
		}
		service := fmt.Sprintf("http://127.0.0.1:%d", in.Port)
		key := host + "\n" + path
		if prior, ok := seen[key]; ok {
			if prior != service {
				return nil, "", fmt.Errorf("Tunnel Host 与路径重复：%s%s", host, path)
			}
			continue
		}
		seen[key] = service
		// The original Host can be independently constrained by the WS server.
		originHost := orDefault(in.Transport.V.Host, host)
		routes = append(routes, cdnTunnelRoute{Hostname: host, Path: "^" + regexp.QuoteMeta(path) + "$", Service: service, OriginRequest: &cdnTunnelOrigin{HTTPHostHeader: originHost}})
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Hostname != routes[j].Hostname {
			return routes[i].Hostname < routes[j].Hostname
		}
		return routes[i].Path < routes[j].Path
	})
	if len(routes) == 0 {
		return nil, "", fmt.Errorf("请先为同一台节点添加已启用的 Tunnel WS 入站")
	}
	return append(routes, cdnTunnelRoute{Service: "http_status:404"}), node, nil
}

func (s *Server) cdnTunnelConfig(c *gin.Context) {
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if !cdnTunnel(&p) {
		badRequest(c, fmt.Errorf("此配置档未启用 Tunnel 模式"))
		return
	}
	routes, node, err := s.tunnelRoutes(&p)
	if err != nil {
		badRequest(c, err)
		return
	}
	cfg := struct {
		Tunnel      string           `yaml:"tunnel"`
		Credentials string           `yaml:"credentials-file"`
		Ingress     []cdnTunnelRoute `yaml:"ingress"`
	}{p.TunnelID, "/etc/cloudflared/" + p.TunnelID + ".json", routes}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(200, gin.H{"nodeId": node, "tunnelId": p.TunnelID, "dnsTarget": p.TunnelID + ".cfargotunnel.com", "routes": routes, "config": string(raw)})
}

func (s *Server) syncCDNTunnel(ctx context.Context, cf *cfClient, zone string, p *store.CDNProfile) (cdnSyncResult, error) {
	res := cdnSyncResult{Records: []string{}}
	routes, _, err := s.tunnelRoutes(p)
	if err != nil {
		return res, err
	}
	hosts := map[string]bool{}
	for _, r := range routes {
		if r.Hostname != "" {
			hosts[r.Hostname] = true
		}
	}
	target := p.TunnelID + ".cfargotunnel.com"
	// An existing VPS A/AAAA record conflicts with a tunnel CNAME. Only
	// migrate records pointing at this node; never erase an unrelated site.
	allowed := map[string]bool{}
	var nodes []store.Node
	if err := s.st.DB.Model(&store.Node{}).Select("nodes.*").Joins("JOIN inbounds ON inbounds.node_id = nodes.id JOIN endpoints ON endpoints.inbound_id = inbounds.id").Where("endpoints.cdn_profile_id = ?", p.ID).Find(&nodes).Error; err != nil {
		return res, err
	}
	for _, n := range nodes {
		for _, ip := range []string{nodeOrigin(&n), n.PublicIPv4, n.PublicIPv6} {
			if ip != "" {
				allowed[ip] = true
			}
		}
	}
	replace := map[string]map[string]bool{}
	previous := map[string][]cfRecord{}
	for host := range hosts {
		records, err := cf.records(ctx, zone, host)
		if err != nil {
			return res, err
		}
		previous[host] = records
		for _, r := range records {
			if r.Type != "A" && r.Type != "AAAA" {
				continue
			}
			if !allowed[r.Content] {
				return res, fmt.Errorf("%s 有非节点的 %s 记录，请先在 Cloudflare 核实并移除冲突记录", host, r.Type)
			}
			if replace[host] == nil {
				replace[host] = map[string]bool{}
			}
			replace[host][r.Type] = true
		}
	}
	names := []string{}
	for host := range hosts {
		names = append(names, host)
	}
	sort.Strings(names)
	for _, host := range names {
		restore := func(cause error) error {
			// Cloudflare cannot hold A/AAAA alongside the new CNAME. Restore
			// old managed records if any step in that replacement fails.
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			for _, r := range previous[host] {
				if r.Type == "A" || r.Type == "AAAA" {
					if err := cf.setRecord(rollbackCtx, zone, host, r.Type, r.Content, r.Proxied, 1); err != nil {
						return fmt.Errorf("%w；恢复原 DNS 记录也失败：%v", cause, err)
					}
				}
			}
			return cause
		}
		for _, typ := range []string{"A", "AAAA"} {
			if replace[host][typ] {
				if err := cf.deleteRecords(ctx, zone, host, typ); err != nil {
					return res, restore(err)
				}
			}
		}
		if err := cf.setRecord(ctx, zone, host, "CNAME", target, true, 1); err != nil {
			return res, restore(cfStep("写 Tunnel DNS "+host, "Zone.DNS 编辑", err))
		}
		res.Records = append(res.Records, host+" → "+target)
	}
	s.warnWrite("tunnel zone", s.st.DB.Model(p).Updates(map[string]any{"zone_id": zone, "last_error": ""}).Error)
	return res, nil // Never change the zone SSL mode or issue origin certificates.
}
