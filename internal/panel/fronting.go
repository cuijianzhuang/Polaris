package panel

import (
	"strconv"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// CDN fronting ("Agent 内置 Caddy 统一回源"): inbounds reached through a CDN
// profile with CaddyFronting share one public TLS port. Caddy terminates TLS
// and routes by path; the inbounds move to loopback ports without TLS.

const frontInternalBase = 40000

type frontInfo struct {
	Port   int
	Domain string
}

// frontFor reports whether an inbound is fronted by Caddy, and how.
func (s *Server) frontFor(inboundID string) (frontInfo, bool) {
	var eps []store.Endpoint
	s.st.DB.Where("inbound_id = ? AND kind = ?", inboundID, "cdn").Find(&eps)
	for _, ep := range eps {
		var p store.CDNProfile
		if s.st.DB.First(&p, "id = ?", ep.CDNProfileID).Error != nil || !p.CaddyFronting {
			continue
		}
		port := ep.Port
		if port == 0 && len(p.Ports.V) > 0 {
			port = p.Ports.V[0]
		}
		if port == 0 {
			port = 443
		}
		return frontInfo{Port: port, Domain: orDefault(ep.Host, p.OriginDomain)}, true
	}
	return frontInfo{}, false
}

// buildFronts rewrites fronted inbounds in the spec and returns Caddy rules.
func (s *Server) buildFronts(spec *model.NodeSpec) []model.FrontRule {
	var rules []model.FrontRule
	fronted := map[int]frontInfo{}
	for i := range spec.Inbounds {
		in := &spec.Inbounds[i]
		if !in.Enabled || !in.CDNCapable() || in.Security.Type != model.SecTLS || in.Security.CertPEM == "" {
			continue
		}
		if fi, ok := s.frontFor(in.ID); ok {
			fronted[i] = fi
		}
	}
	if len(fronted) == 0 {
		return nil
	}
	// A non-fronted TCP inbound on the public port blocks fronting on that port.
	blocked := map[int]string{}
	for i, in := range spec.Inbounds {
		if _, ok := fronted[i]; !ok && in.Enabled && in.UsesTCP() {
			blocked[in.Port] = in.Tag
		}
	}
	n := 0
	for i := range spec.Inbounds {
		fi, ok := fronted[i]
		if !ok {
			continue
		}
		in := &spec.Inbounds[i]
		if tag, bad := blocked[fi.Port]; bad {
			s.log.Warn("cdn fronting skipped: port in use", "inbound", in.Tag, "port", fi.Port, "by", tag)
			continue
		}
		path := in.Transport.Path
		if in.Transport.Type == model.TransGRPC {
			path = "/" + in.Transport.ServiceName
		}
		internal := frontInternalBase + n
		n++
		rules = append(rules, model.FrontRule{
			Domain: fi.Domain, Port: fi.Port, Path: path, Upstream: "127.0.0.1:" + strconv.Itoa(internal),
			H2C:     in.Transport.Type == model.TransGRPC || in.Transport.Type == model.TransXHTTP,
			CertPEM: in.Security.CertPEM, KeyPEM: in.Security.KeyPEM, Inbound: in.Tag, Kind: in.Transport.Type,
		})
		in.Listen, in.Port = "127.0.0.1", internal
		in.Security = model.SecuritySpec{Type: model.SecNone}
	}
	return rules
}
