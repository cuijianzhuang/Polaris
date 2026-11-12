package cdnkit

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// HTTPPorts are the ports Cloudflare proxies over plain HTTP.
var HTTPPorts = []int{80, 8080, 8880, 2052, 2082, 2086, 2095}

// HTTPSPorts are the ports Cloudflare proxies over HTTPS.
var HTTPSPorts = []int{443, 2053, 2083, 2087, 2096, 8443}

var tunnelIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Scheme is the client-facing scheme of a profile. Empty means HTTPS for
// existing profiles and old backups.
func Scheme(p *store.CDNProfile) string {
	if p.Scheme == "http" {
		return "http"
	}
	return "https"
}

// ClientPorts are the ports clients may connect to on the edge.
func ClientPorts(p *store.CDNProfile) []int {
	if Scheme(p) == "http" {
		return HTTPPorts
	}
	return HTTPSPorts
}

// DefaultPort is the profile's first configured port, or the scheme default.
func DefaultPort(p *store.CDNProfile) int {
	if len(p.Ports.V) > 0 {
		return p.Ports.V[0]
	}
	if Scheme(p) == "http" {
		return 80
	}
	return 443
}

// PortsText lists the client ports the way the admin UI shows them.
func PortsText(p *store.CDNProfile) string {
	parts := make([]string, len(ClientPorts(p)))
	for i, port := range ClientPorts(p) {
		parts[i] = fmt.Sprint(port)
	}
	return strings.Join(parts, "、")
}

// Tunnel reports whether the profile goes through a Cloudflare Tunnel.
func Tunnel(p *store.CDNProfile) bool { return p.OriginMode == "tunnel" }

// ValidTunnelID reports whether id is a Cloudflare Tunnel UUID.
func ValidTunnelID(id string) bool { return tunnelIDRe.MatchString(id) }

// OriginTLS reports whether HTTPS visitors reach the origin over TLS.
// Cloudflare's origin encryption mode affects HTTPS visitors; HTTP visitors
// reach the origin using plain HTTP even under Full / Full (strict).
func OriginTLS(p *store.CDNProfile) bool {
	return OriginTLSAt(p, DefaultPort(p))
}

// OriginTLSAt is OriginTLS for one client port. Flexible only applies to
// HTTPS :443; other HTTPS ports fall back to Full.
func OriginTLSAt(p *store.CDNProfile, port int) bool {
	if Tunnel(p) {
		return false
	}
	return Scheme(p) == "https" && !(p.TLSMode == "flexible" && (p.Provider != "cloudflare" || port == 443))
}

// OriginPort maps a client port to the origin port Cloudflare dials.
func OriginPort(p *store.CDNProfile, port int) int {
	if p.Provider == "cloudflare" && Scheme(p) == "https" && p.TLSMode == "flexible" && port == 443 {
		return 80
	}
	return port
}

// TunnelProblem says why a Tunnel profile cannot serve an inbound ("" = it can).
func TunnelProblem(m *model.Inbound, p *store.CDNProfile, port int) string {
	if p.Provider != "cloudflare" || Scheme(p) != "https" || p.CaddyFronting || !tunnelIDRe.MatchString(p.TunnelID) {
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

// NodePort keeps ports proxied for the selected client scheme; otherwise it
// uses the profile's first port (for fronting / explicit origin rewrites).
func NodePort(p *store.CDNProfile, inboundPort int, fronted bool) int {
	if Tunnel(p) {
		return 443
	}
	if fronted || !slices.Contains(ClientPorts(p), inboundPort) {
		return DefaultPort(p)
	}
	return inboundPort
}

// Problem says why clients could not reach the inbound through the CDN
// profile ("" = they can). epPort is the endpoint's own port override (0 =
// none); fronted is whether Agent's Caddy fronts the inbound.
//
// Client scheme selects the edge ports independently of origin SSL mode.
func Problem(m *model.Inbound, p *store.CDNProfile, epPort int, fronted bool) string {
	if Tunnel(p) {
		return TunnelProblem(m, p, epPort)
	}
	if fronted && Scheme(p) == "http" {
		return "HTTP CDN 不支持内置 Caddy 统一回源，请使用 HTTPS"
	}
	tls := m.Security.Type == model.SecTLS
	if fronted && !tls {
		return "Caddy 统一回源需要入站开启 TLS 并配置证书"
	}
	if p.Provider != "cloudflare" {
		return ""
	}
	port := epPort
	if port == 0 {
		port = NodePort(p, m.Port, fronted)
	}
	if !fronted && epPort == 0 && m.Port != OriginPort(p, port) {
		return fmt.Sprintf("入站端口 %d 不是 Cloudflare 的 %s 端口，不能自动映射到 %d：请使用受支持的入站端口或 Caddy；已配置外部回源端口改写时，需在入口明确填写客户端端口", m.Port, strings.ToUpper(Scheme(p)), port)
	}
	if !slices.Contains(ClientPorts(p), port) {
		return fmt.Sprintf("客户端会经 Cloudflare 连 %d 端口，但 %s 只支持 %s：请修改入口端口", port, strings.ToUpper(Scheme(p)), PortsText(p))
	}
	if m.Transport.Type == model.TransGRPC && Scheme(p) == "http" {
		return "Cloudflare gRPC 需要 HTTPS CDN 和 443 端口，请选择 HTTPS 配置档"
	}
	if m.Transport.Type == model.TransGRPC && port != 443 {
		return fmt.Sprintf("Cloudflare 只在 443 端口代理 gRPC，当前客户端会连 %d 端口：把入站端口改为 443、开启配置档的「Caddy 统一回源」，或改用 WS", port)
	}
	if Scheme(p) == "http" {
		if tls {
			return "HTTP CDN 使用明文 HTTP 回源，入站应关闭 TLS，或改用 HTTPS CDN"
		}
	} else if !OriginTLSAt(p, port) {
		if tls {
			return "SSL 模式 Flexible 下 Cloudflare 用明文 HTTP 回源，开了 TLS 的入站接不上：把配置档的 SSL 模式改为 Full 或 Full (strict)"
		}
	} else if !tls {
		return "SSL 模式 Full / Full (strict) 下 Cloudflare 用 HTTPS 回源，入站需要开启 TLS（或开启配置档的「Caddy 统一回源」）"
	}
	return ""
}
