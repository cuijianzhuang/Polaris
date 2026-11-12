package panel

import (
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/panel/cdnkit"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Problems that make an inbound unreachable through a CDN profile even
// though its transport can go through a CDN.

// cdnEndpointProblem is cdnProblem for an inbound stored on a node, where
// another inbound may hold the port Caddy fronting needs.
func (s *Server) cdnEndpointProblem(m *model.Inbound, inboundID string, p *store.CDNProfile, epPort int) string {
	if !p.CaddyFronting {
		return cdnkit.Problem(m, p, epPort, false)
	}
	port := epPort
	if port == 0 {
		port = cdnkit.NodePort(p, m.Port, true)
	}
	tag := s.frontBlockerOn(m.NodeID, inboundID, port)
	problem := cdnkit.Problem(m, p, epPort, tag == "")
	if tag != "" && problem != "" {
		return fmt.Sprintf("Caddy 统一回源用不了（%d 端口被入站 %s 占用），而直接回源也不行：%s", port, tag, problem)
	}
	return problem
}

// cdnCertWarning: under Full (strict) Cloudflare checks that the origin
// certificate covers the origin domain; a certificate for the node's own
// domain (or a self-signed one) fails with error 526.
func cdnCertWarning(m *model.Inbound, p *store.CDNProfile) string {
	if cdnkit.Tunnel(p) {
		return ""
	}
	if cdnkit.Scheme(p) == "http" || p.Provider != "cloudflare" || m.Security.Type != model.SecTLS {
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
	return profileID != "" && m.CDNCapable() && db.First(&p, "id = ?", profileID).Error == nil && cdnkit.Problem(m, &p, port, p.CaddyFronting) == ""
}
