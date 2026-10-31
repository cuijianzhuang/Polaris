package panel

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func cdnReportTarget(req cdnIPReportReq, p *store.CDNProfile) (int, string, error) {
	if req.Port == 0 && req.Scheme == "" && cdnScheme(p) == "https" {
		return 0, "", nil
	}
	if req.Scheme != "" && req.Scheme != cdnScheme(p) {
		return 0, "", fmt.Errorf("测速协议与 CDN 配置档不一致")
	}
	if req.Port < 1 || req.Port > 65535 || (p.Provider == "cloudflare" && !slices.Contains(cdnClientPorts(p), req.Port)) {
		return 0, "", fmt.Errorf("请上报当前 CDN 协议支持的实际测速端口")
	}
	return req.Port, cdnScheme(p), nil
}

func effectiveCDNReportPort(r store.CDNIPReport) int {
	if r.Port == 0 {
		return 443
	}
	return r.Port
}
func effectiveCDNReportScheme(r store.CDNIPReport) string {
	if r.Scheme == "" {
		return "https"
	}
	return r.Scheme
}

// A list measured on a different port remains a fallback, never evidence that
// this port was measured. Matching reports replace it when available.
func (s *Server) cdnAddressesForPort(p *store.CDNProfile, port int) []string {
	if port == cdnDefaultPort(p) {
		return p.PreferredIPs.V
	}
	list := p.PreferredIPs.V
	reports := cdnReportsForPort(s.st.DB, p, port, cdnSelectionCutoff(p), "")
	if len(reports) > 0 {
		list = withPinned(mergeCDNReports(reports), p.PinnedHosts.V)
	}
	return list
}

// A keep policy retains the last results for each port and carrier, rather
// than silently replacing an expired port with the default port's list.
func cdnSelectionCutoff(p *store.CDNProfile) time.Time {
	if p.StalePolicy == "keep" {
		return time.Time{}
	}
	return time.Now().Add(-cdnStaleAfter(p))
}

func (s *Server) cdnIPsForPort(p *store.CDNProfile, port int) []string {
	return usableCDNIPsAt(p, s.cdnAddressesForPort(p, port), port)
}

func (s *Server) cdnCheckPorts(p *store.CDNProfile) []int {
	ports := []int{cdnDefaultPort(p)}
	var rows []struct {
		InboundID   string
		InboundPort int
		Port        int
	}
	s.st.DB.Table("endpoints").Select("endpoints.inbound_id,inbounds.port AS inbound_port,endpoints.port").Joins("JOIN inbounds ON inbounds.id = endpoints.inbound_id").Where("endpoints.cdn_profile_id = ?", p.ID).Order("endpoints.id").Scan(&rows)
	for _, row := range rows {
		port := row.Port
		if port == 0 {
			_, fronted := s.frontFor(row.InboundID)
			port = cdnNodePort(p, row.InboundPort, fronted && p.CaddyFronting)
		}
		if port > 0 && !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	return ports
}

func cdnAddressBlocked(p *store.CDNProfile, address string) bool {
	for _, a := range p.BlockedHosts.V {
		ai, ae := netip.ParseAddr(a)
		bi, be := netip.ParseAddr(address)
		if ae == nil && be == nil && ai.Unmap() == bi.Unmap() {
			return true
		}
		if strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(address, ".")) {
			return true
		}
	}
	return false
}

// This selection is shared by subscription output and the operator preview.
func (s *Server) selectedCDNIPs(p *store.CDNProfile, port int, carrier string, audience ...cdnAudience) []string {
	if len(audience) > 0 && len(audience[0].Personal[p.ID]) > 0 {
		return usableCDNIPsAt(p, audience[0].Personal[p.ID], port)
	}
	ips := s.cdnAudienceIPs(p, port, carrier, audienceRegion(audience))
	if ips == nil {
		ips = s.cdnIPsForPort(p, port)
	}
	if p.IPTopN > 0 {
		var automatic, pinned []string
		for _, a := range ips {
			if slices.Contains(p.PinnedHosts.V, a) {
				pinned = append(pinned, a)
			} else {
				automatic = append(automatic, a)
			}
		}
		ips = append(automatic[:min(len(automatic), p.IPTopN)], pinned...)
	}
	return ips
}

func usableCDNIPsAt(p *store.CDNProfile, list []string, port int) []string {
	list = slices.DeleteFunc(slices.Clone(list), func(a string) bool { return cdnAddressBlocked(p, a) })
	if !p.NodeCheck {
		return list
	}
	var bad []string
	if check, ok := p.PortChecks.V[strconv.Itoa(port)]; ok {
		bad = check.BadIPs
	} else if port == cdnDefaultPort(p) {
		bad = p.BadIPs.V
	}
	out := slices.DeleteFunc(slices.Clone(list), func(ip string) bool { return slices.Contains(bad, ip) })
	if len(out) == 0 {
		return list
	} // preserve the existing all-unreachable fallback
	return out
}
