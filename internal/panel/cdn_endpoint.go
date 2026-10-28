package panel

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// cdnTarget is where a client of a cdn endpoint connects, as the
// subscription tells it (see expandEndpoint).
type cdnTarget struct {
	Server string `json:"server"`
	Port   int    `json:"port"`
	SNI    string `json:"sni"`
	Host   string `json:"host"`
	Path   string `json:"path"`
}

func (s *Server) cdnTargetFor(ep *store.Endpoint, m *model.Inbound, p *store.CDNProfile) cdnTarget {
	_, fronted := s.frontFor(m.ID)
	host := orDefault(ep.Host, p.OriginDomain)
	t := cdnTarget{Server: host, Port: cdnNodePort(p, m.Port, fronted && p.CaddyFronting), SNI: host, Host: host, Path: m.Transport.Path}
	if m.Transport.Type == model.TransGRPC {
		t.Path = "/" + m.Transport.ServiceName
	}
	if p.ExpandPerIP {
		if ips := usableCDNIPs(p); len(ips) > 0 {
			t.Server = ips[0]
		}
	}
	if ep.Address != "" {
		t.Server = ep.Address
	}
	if ep.Port > 0 {
		t.Port = ep.Port
	}
	if ep.SNI != "" {
		t.SNI = ep.SNI
	}
	if ep.Path != "" {
		t.Path = ep.Path
	}
	if t.Path == "" {
		t.Path = "/"
	}
	return t
}

// cdnEndpointWarnings lists what keeps clients of a cdn endpoint from
// connecting, as far as the configuration shows.
func (s *Server) cdnEndpointWarnings(ep *store.Endpoint, row *store.Inbound, p *store.CDNProfile) []string {
	m := s.toModelInbound(row)
	if !m.CDNCapable() {
		return []string{fmt.Sprintf("%s+%s 不能走 CDN，订阅里不会出现这个入口", m.Transport.Type, m.Security.Type)}
	}
	var out []string
	if problem := s.cdnEndpointProblem(&m, row.ID, p, ep.Port); problem != "" {
		out = append(out, problem)
	}
	if w := s.cdnCertProblem(&m, p, orDefault(ep.Host, p.OriginDomain)); w != "" {
		out = append(out, w)
	}
	return out
}

// cdnCertProblem is cdnCertWarning for a stored inbound: its certificate
// (a Cloudflare Origin one, or one for the origin host) may already fit.
func (s *Server) cdnCertProblem(m *model.Inbound, p *store.CDNProfile, host string) string {
	if p.Provider != "cloudflare" || m.Security.Type != model.SecTLS || (p.TLSMode != "" && p.TLSMode != "full_strict") {
		return ""
	}
	var c store.Certificate
	if m.Security.CertificateID != "" && s.st.DB.Select("provider", "domain").First(&c, "id = ?", m.Security.CertificateID).Error == nil {
		if c.Provider == "cf-origin" || covers(c.Domain, host) {
			return ""
		}
	}
	pp := *p
	pp.OriginDomain = host
	return cdnCertWarning(m, &pp)
}

type endpointView struct {
	store.Endpoint
	Target   *cdnTarget `json:"target,omitempty"`
	Warnings []string   `json:"warnings,omitempty"`
}

func (s *Server) endpointViews(row *store.Inbound, es []store.Endpoint) []endpointView {
	out := make([]endpointView, 0, len(es))
	m := s.toModelInbound(row)
	for i := range es {
		v := endpointView{Endpoint: es[i]}
		var p store.CDNProfile
		if es[i].Kind == "cdn" && s.st.DB.First(&p, "id = ?", es[i].CDNProfileID).Error == nil {
			t := s.cdnTargetFor(&es[i], &m, &p)
			v.Target, v.Warnings = &t, s.cdnEndpointWarnings(&es[i], row, &p)
		}
		out = append(out, v)
	}
	return out
}

// checkEndpoint is POST /inbounds/:id/endpoints/:eid/check: the panel
// connects like a client of the cdn endpoint and explains what fails.
func (s *Server) checkEndpoint(c *gin.Context) {
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var ep store.Endpoint
	if err := s.st.DB.First(&ep, "id = ? AND inbound_id = ?", c.Param("eid"), row.ID).Error; err != nil {
		dbError(c, err)
		return
	}
	var p store.CDNProfile
	if ep.Kind != "cdn" || s.st.DB.First(&p, "id = ?", ep.CDNProfileID).Error != nil {
		badRequest(c, errors.New("只能检测 CDN 入口"))
		return
	}
	m := s.toModelInbound(&row)
	t := s.cdnTargetFor(&ep, &m, &p)
	var node store.Node
	s.st.DB.First(&node, "id = ?", row.NodeID)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	res := probeCDN(ctx, t, m.Transport.Type, nodeOrigin(&node))
	if !res.OK && (res.Status == 521 || res.Status == 522 || res.Status == 523) {
		// Cloudflare could not reach the origin: see where its record
		// points and whether the node answers on that port at all.
		dctx, dcancel := context.WithTimeout(context.Background(), 15*time.Second)
		s.diagnoseOrigin(dctx, &res, &p, &node, t)
		dcancel()
	}
	res.Warnings = s.cdnEndpointWarnings(&ep, &row, &p)
	c.JSON(http.StatusOK, res)
}

// diagnoseOrigin adds what the panel sees of the hop Cloudflare failed:
// the DNS record (with the profile's API token) and a direct connection
// to the node's port. Cloudflare reaches the origin on the client's port.
func (s *Server) diagnoseOrigin(ctx context.Context, r *cdnCheck, p *store.CDNProfile, node *store.Node, t cdnTarget) {
	nodeIPs := []string{}
	for _, ip := range []string{nodeOrigin(node), node.PublicIPv4, node.PublicIPv6} {
		if ip = strings.TrimSpace(ip); ip != "" && !slices.Contains(nodeIPs, ip) {
			nodeIPs = append(nodeIPs, ip)
		}
	}
	if token := s.st.Box.MustOpen(p.APIToken); token != "" {
		cf := newCF(token)
		zone := p.ZoneID
		if zone == "" {
			zone, _ = cf.zoneFor(ctx, p.OriginDomain)
		}
		if zone != "" {
			// Under Flexible (or Off) Cloudflare reaches the origin over plain
			// HTTP on an HTTP port (80, or the HTTP twin of the client's
			// port), where nothing listens: 521 although the TLS port answers.
			if mode, err := cf.sslMode(ctx, zone); err == nil {
				r.Steps = append(r.Steps, "Cloudflare SSL/TLS 模式："+mode)
				if mode == "flexible" || mode == "off" {
					r.Message = fmt.Sprintf("Cloudflare 的 SSL/TLS 模式是 %s：Cloudflare 用明文 HTTP 连节点的 HTTP 端口（80 或对应的 HTTP 端口，而不是 %d），那里没有程序监听，所以报 521。到 Cloudflare 后台 SSL/TLS 把模式改为 Full（或在配置档选 Full 后点「同步 DNS」）", mode, t.Port)
					return
				}
			}
			if recs, err := cf.records(ctx, zone, t.SNI); err == nil {
				var contents []string
				match := false
				for _, rec := range recs {
					contents = append(contents, rec.Type+" "+rec.Content)
					match = match || slices.Contains(nodeIPs, rec.Content)
				}
				if len(recs) == 0 {
					r.Steps = append(r.Steps, fmt.Sprintf("Cloudflare 里没有 %s 的 DNS 记录", t.SNI))
				} else {
					r.Steps = append(r.Steps, fmt.Sprintf("Cloudflare DNS：%s → %s", t.SNI, strings.Join(contents, ", ")))
				}
				if len(recs) > 0 && !match && recs[0].Type != "CNAME" {
					r.Message = fmt.Sprintf("%s 的 DNS 记录指向 %s，不是节点 %s（%s）：Cloudflare 连到了别的机器。点配置档的「同步 DNS」，或在 Cloudflare 改记录；多台节点共用一个配置档时，给每个入口填不同的 Host（如 sg-%s）",
						t.SNI, strings.Join(contents, ", "), node.Name, strings.Join(nodeIPs, " / "), p.OriginDomain)
					return
				}
			} else {
				r.Steps = append(r.Steps, "读取 Cloudflare DNS 记录失败："+err.Error())
			}
		}
	} else {
		r.Steps = append(r.Steps, "配置档没填 API Token，无法读出 DNS 记录指向哪里；请在 Cloudflare 后台确认它指向 "+strings.Join(nodeIPs, " / "))
	}
	if len(nodeIPs) == 0 {
		return
	}
	addr := net.JoinHostPort(nodeIPs[0], strconv.Itoa(t.Port))
	conn, err := (&net.Dialer{Timeout: 6 * time.Second}).DialContext(ctx, "tcp", addr)
	switch {
	case err != nil && strings.Contains(err.Error(), "refused"):
		r.Steps = append(r.Steps, "面板直连 "+addr+"：连接被拒绝")
		r.Message += fmt.Sprintf("。面板直连 %s 也被拒绝：节点上 %d 端口没有程序在监听（看节点卡片的版本号是否已应用、内核是否在运行），或系统防火墙（iptables / ufw，Oracle 镜像默认有 REJECT 规则）拒绝了这个端口", addr, t.Port)
	case err != nil:
		r.Steps = append(r.Steps, "面板直连 "+addr+"："+err.Error())
		r.Message += fmt.Sprintf("。面板直连 %s 也连不上（%v）：检查云服务商安全组 / 防火墙是否放行 %d 端口", addr, err, t.Port)
	default:
		_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
		tc := tls.Client(conn, &tls.Config{ServerName: t.SNI, InsecureSkipVerify: true}) // only asks whether TLS is served there
		herr := tc.HandshakeContext(ctx)
		conn.Close()
		if herr != nil {
			r.Steps = append(r.Steps, "面板直连 "+addr+"：TCP 通，TLS 握手失败："+herr.Error())
			return
		}
		r.Steps = append(r.Steps, "面板直连 "+addr+"：正常（TLS 握手成功）")
		if isLocalIP(nodeIPs[0]) {
			r.Message += fmt.Sprintf("。面板和节点在同一台机器上，面板直连 %s 走的是本机、不经过防火墙，所以不能说明外部能连：检查这台机器的防火墙（iptables / ufw / nftables）、fail2ban / CrowdSec 是否拦了 Cloudflare 的 IP，以及云服务商的安全组是否放行 %d 端口", addr, t.Port)
		} else {
			r.Message += fmt.Sprintf("。但面板直连 %s 是通的：节点本身没问题，多半是 DNS 记录指向了别的 IP，或防火墙（iptables / ufw、fail2ban / CrowdSec）只拦了 Cloudflare 的 IP 段", addr)
		}
	}
}

type cdnCheck struct {
	OK       bool     `json:"ok"`
	Status   int      `json:"status,omitempty"`
	Message  string   `json:"message"`
	Steps    []string `json:"steps"`
	Warnings []string `json:"warnings,omitempty"`
}

// cdnProbeRoots overrides the system roots in tests.
var cdnProbeRoots *x509.CertPool

// Cloudflare's 52x errors name the hop that failed.
var cfErrors = map[int]string{
	520: "节点返回了 Cloudflare 看不懂的响应：入站协议或路径和订阅不一致",
	521: "Cloudflare 连节点被拒绝：节点上这个端口没有程序在监听（入站还没下发、内核没启动），或防火墙拒绝了 Cloudflare",
	522: "Cloudflare 连节点超时：云服务商的安全组 / 防火墙没放行这个端口，或 DNS 记录指向的 IP 不对",
	523: "Cloudflare 找不到节点：DNS 记录里的地址不可达",
	524: "节点响应超时：内核卡住或负载过高",
	525: "Cloudflare 和节点 SSL 握手失败：节点这个端口上不是 TLS 服务（入站没开 TLS），或 SSL 模式与入站不匹配",
	526: "节点证书无效：SSL 模式是 Full (strict)，而证书不包含回源域名或是自签证书。在配置档点「同步 DNS」换成 Origin 证书，或把 Cloudflare 的 SSL 模式改为 Full",
	530: "Cloudflare 解析不了回源（错误 1016）：回源域名的 DNS 记录不对",
}

func probeCDN(ctx context.Context, t cdnTarget, transport model.Transport, nodeIP string) cdnCheck {
	r := cdnCheck{Steps: []string{}}
	step := func(f string, a ...any) { r.Steps = append(r.Steps, fmt.Sprintf(f, a...)) }
	fail := func(f string, a ...any) cdnCheck { r.Message = fmt.Sprintf(f, a...); return r }

	// 1. The origin domain must resolve, through Cloudflare's proxy.
	if net.ParseIP(t.SNI) == nil {
		addrs, err := net.DefaultResolver.LookupHost(ctx, t.SNI)
		if err != nil || len(addrs) == 0 {
			return fail("域名 %s 没有 DNS 记录：在 Cloudflare 添加一条指向节点 IP 的记录并开启代理（橙色云朵），或在配置档点「同步 DNS」", t.SNI)
		}
		step("%s 解析到 %s", t.SNI, strings.Join(addrs, ", "))
		for _, a := range addrs {
			if nodeIP != "" && a == nodeIP {
				return fail("%s 直接解析到节点 IP %s，没有经过 Cloudflare：在 Cloudflare 的 DNS 里把这条记录的代理打开（橙色云朵）", t.SNI, a)
			}
		}
	}

	// 2. TLS to Cloudflare on the client's port.
	addr := net.JoinHostPort(t.Server, strconv.Itoa(t.Port))
	d := &net.Dialer{Timeout: 8 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fail("连不上 %s：%v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
	tc := tls.Client(conn, &tls.Config{ServerName: t.SNI, NextProtos: []string{"http/1.1"}, RootCAs: cdnProbeRoots})
	if err := tc.HandshakeContext(ctx); err != nil {
		return fail("和 %s 的 TLS 握手失败（SNI %s）：%v。检查域名是否接入 Cloudflare、端口是否为 Cloudflare 的 HTTPS 端口", addr, t.SNI, err)
	}
	step("TLS 连上 %s（SNI %s）", addr, t.SNI)

	// 3. A WebSocket upgrade on the inbound's path, through to the origin.
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	req := "GET " + t.Path + " HTTP/1.1\r\nHost: " + t.Host + "\r\nUser-Agent: Polaris-check\r\n"
	if transport == model.TransWS || transport == model.TransHTTPUpgrade {
		req += "Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\n"
	} else {
		req += "Connection: close\r\n"
	}
	if _, err := tc.Write([]byte(req + "\r\n")); err != nil {
		return fail("发送请求失败：%v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		return fail("读取响应失败：%v", err)
	}
	resp.Body.Close()
	r.Status = resp.StatusCode
	viaCF := resp.Header.Get("Cf-Ray") != "" || strings.EqualFold(resp.Header.Get("Server"), "cloudflare")
	step("HTTP %d%s", resp.StatusCode, map[bool]string{true: "（经 Cloudflare）", false: ""}[viaCF])
	if msg, ok := cfErrors[resp.StatusCode]; ok && viaCF {
		return fail("错误 %d：%s", resp.StatusCode, msg)
	}
	switch {
	case resp.StatusCode == http.StatusSwitchingProtocols:
		r.OK = true
		r.Message = "连通：客户端 → Cloudflare → 节点入站都通了"
	case transport != model.TransWS && transport != model.TransHTTPUpgrade && resp.StatusCode < 500:
		r.OK = true
		r.Message = fmt.Sprintf("Cloudflare 已连到节点（HTTP %d）；%s 无法在这里完整握手，客户端仍连不上请看内核日志", resp.StatusCode, transport)
	case resp.StatusCode == http.StatusForbidden && viaCF:
		r.Message = "被 Cloudflare 拦截（403）：检查 WAF、Bot Fight Mode 或防火墙规则"
	case !viaCF:
		r.Message = fmt.Sprintf("响应没有经过 Cloudflare（HTTP %d）：这个地址或域名没走 Cloudflare 代理", resp.StatusCode)
	default:
		r.Message = fmt.Sprintf("已到达节点，但 WebSocket 握手失败（HTTP %d）：入站路径或 Host 和订阅不一致，或入站不是 WS", resp.StatusCode)
	}
	return r
}

// isLocalIP reports whether ip is an address of this machine (the panel
// runs on the node).
func isLocalIP(ip string) bool {
	want := net.ParseIP(ip)
	if want == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(want) {
			return true
		}
	}
	return false
}
