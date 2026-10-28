package panel

import (
	"fmt"
	"slices"
	"strings"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Problems that make an inbound unreachable through a CDN profile even
// though its transport can go through a CDN.

// cdnProblem says why clients could not reach the inbound through the CDN
// profile ("" = they can). epPort is the endpoint's own port override (0 =
// none); fronted is whether Agent's Caddy fronts the inbound.
//
// Clients always speak TLS to the CDN (see expandEndpoint), so with
// Cloudflare: the port must be one of its HTTPS ports, gRPC works only on
// 443, Full / Full (strict) reach the origin over HTTPS (the inbound, or
// Caddy, must terminate TLS) and Flexible reaches it over plain HTTP.
func cdnProblem(m *model.Inbound, p *store.CDNProfile, epPort int, fronted bool) string {
	tls := m.Security.Type == model.SecTLS
	if fronted && !tls {
		return "Caddy 统一回源需要入站开启 TLS 并配置证书"
	}
	if p.Provider != "cloudflare" {
		return ""
	}
	port := epPort
	if port == 0 {
		port = cdnNodePort(p, m.Port, fronted)
	}
	if !slices.Contains(cloudflareHTTPSPorts, port) {
		return fmt.Sprintf("客户端会经 Cloudflare 连 %d 端口，但 Cloudflare 只在 443、2053、2083、2087、2096、8443 上代理 HTTPS：把入站端口改成其中之一", port)
	}
	if m.Transport.Type == model.TransGRPC && port != 443 {
		return fmt.Sprintf("Cloudflare 只在 443 端口代理 gRPC，当前客户端会连 %d 端口：把入站端口改为 443、开启配置档的「Caddy 统一回源」，或改用 WS", port)
	}
	if p.TLSMode == "flexible" {
		if tls {
			return "SSL 模式 Flexible 下 Cloudflare 用明文 HTTP 回源，开了 TLS 的入站接不上：把配置档的 SSL 模式改为 Full 或 Full (strict)"
		}
	} else if !tls {
		return "SSL 模式 Full / Full (strict) 下 Cloudflare 用 HTTPS 回源，入站需要开启 TLS（或开启配置档的「Caddy 统一回源」）"
	}
	return ""
}

// cdnEndpointProblem is cdnProblem for an inbound stored on a node, where
// another inbound may hold the port Caddy fronting needs.
func (s *Server) cdnEndpointProblem(m *model.Inbound, inboundID string, p *store.CDNProfile, epPort int) string {
	if !p.CaddyFronting {
		return cdnProblem(m, p, epPort, false)
	}
	port := epPort
	if port == 0 {
		port = cdnNodePort(p, m.Port, true)
	}
	tag := s.frontBlockerOn(m.NodeID, inboundID, port)
	problem := cdnProblem(m, p, epPort, tag == "")
	if tag != "" && problem != "" {
		return fmt.Sprintf("Caddy 统一回源用不了（%d 端口被入站 %s 占用），而直接回源也不行：%s", port, tag, problem)
	}
	return problem
}

// cdnCertWarning: under Full (strict) Cloudflare checks that the origin
// certificate covers the origin domain; a certificate for the node's own
// domain (or a self-signed one) fails with error 526.
func cdnCertWarning(m *model.Inbound, p *store.CDNProfile) string {
	if p.Provider != "cloudflare" || m.Security.Type != model.SecTLS {
		return ""
	}
	if p.TLSMode != "" && p.TLSMode != "full_strict" {
		return ""
	}
	if covers(m.Security.ServerName, p.OriginDomain) {
		return ""
	}
	return fmt.Sprintf("SSL 模式 Full (strict) 要求证书包含回源域名 %s，本入站证书签给 %s：在 CDN 配置档点「同步 DNS」换成 Cloudflare Origin 证书，或把 SSL 模式改为 Full",
		p.OriginDomain, orDefault(m.Security.ServerName, "（未设置）"))
}

// covers reports whether a certificate name (possibly *.zone) matches host.
func covers(name, host string) bool {
	name, host = strings.ToLower(name), strings.ToLower(host)
	if name == host {
		return true
	}
	if strings.HasPrefix(name, "*.") {
		rest, ok := strings.CutSuffix(host, name[1:])
		return ok && rest != "" && !strings.Contains(rest, ".")
	}
	return false
}

// itemWantsCDN reports whether a template item publishes through a CDN.
func itemWantsCDN(it store.TemplateItem) bool {
	for _, e := range it.Endpoints {
		if e.Kind == "cdn" {
			return true
		}
	}
	return false
}

// cdnEndpointUsable reports whether a template may add a cdn endpoint for
// the profile; one clients could not reach is left out (preview says why).
func cdnEndpointUsable(db *gorm.DB, m *model.Inbound, profileID string, port int) bool {
	var p store.CDNProfile
	return profileID != "" && m.CDNCapable() && db.First(&p, "id = ?", profileID).Error == nil && cdnProblem(m, &p, port, p.CaddyFronting) == ""
}
