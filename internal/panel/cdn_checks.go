package panel

import (
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Problems that make an inbound unreachable through a CDN profile even
// though its transport can go through a CDN.

// cdnPortProblem: Cloudflare proxies gRPC only on port 443. epPort is the
// endpoint's own port override (0 = none).
func cdnPortProblem(m *model.Inbound, p *store.CDNProfile, epPort int) string {
	if p.Provider != "cloudflare" || m.Transport.Type != model.TransGRPC {
		return ""
	}
	port := epPort
	if port == 0 {
		port = cdnNodePort(p, m.Port)
	}
	if port == 443 {
		return ""
	}
	return fmt.Sprintf("Cloudflare 只在 443 端口代理 gRPC，当前客户端会连 %d 端口：把入站端口改为 443、开启配置档的「Caddy 统一回源」，或改用 WS", port)
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
	return profileID != "" && m.CDNCapable() && db.First(&p, "id = ?", profileID).Error == nil && cdnPortProblem(m, &p, port) == ""
}
