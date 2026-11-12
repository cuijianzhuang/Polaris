package panel

import (
	"slices"
	"time"

	"github.com/cuijianzhuang/polaris/internal/panel/cdnkit"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A list measured on a different port remains a fallback, never evidence that
// this port was measured. Matching reports replace it when available.
func (s *Server) cdnAddressesForPort(p *store.CDNProfile, port int) []string {
	if port == cdnkit.DefaultPort(p) {
		return p.PreferredIPs.V
	}
	list := p.PreferredIPs.V
	reports := cdnReportsForPort(s.st.DB, p, port, cdnSelectionCutoff(p), "")
	if len(reports) > 0 {
		list = cdnkit.MergedReportIPs(p, reports)
	}
	return list
}

// A keep policy retains the last results for each port and carrier, rather
// than silently replacing an expired port with the default port's list.
func cdnSelectionCutoff(p *store.CDNProfile) time.Time {
	if p.StalePolicy == "keep" {
		return time.Time{}
	}
	return time.Now().UTC().Add(-cdnkit.StaleAfter(p))
}

func (s *Server) cdnIPsForPort(p *store.CDNProfile, port int) []string {
	return cdnkit.UsableIPsAt(p, s.cdnAddressesForPort(p, port), port)
}

func (s *Server) cdnCheckPorts(p *store.CDNProfile) []int {
	ports := []int{cdnkit.DefaultPort(p)}
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
			port = cdnkit.NodePort(p, row.InboundPort, fronted && p.CaddyFronting)
		}
		if port > 0 && !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	return ports
}

// This selection is shared by subscription output and the operator preview.
func (s *Server) selectedCDNIPs(p *store.CDNProfile, port int, carrier string, audience ...cdnkit.Audience) []string {
	return s.selectedCDNIPsWhere(p, port, carrier, nil, audience...)
}

// Apply endpoint diagnostics before Top N so failed entries do not consume slots.
func (s *Server) selectedCDNIPsWhere(p *store.CDNProfile, port int, carrier string, keep func(string) bool, audience ...cdnkit.Audience) []string {
	filter := func(ips []string) []string {
		if keep == nil {
			return ips
		}
		return slices.DeleteFunc(slices.Clone(ips), func(a string) bool { return !keep(a) })
	}
	if len(audience) > 0 {
		personal := slices.Clone(audience[0].Personal[p.ID])
		for _, address := range personalCDNAutomatic(audience[0].Pools[p.ID], port, cdnkit.Scheme(p), time.Now()) {
			if !slices.Contains(personal, address) {
				personal = append(personal, address)
			}
		}
		personal = slices.DeleteFunc(personal, func(address string) bool { return !personalCDNAllowed(audience[0].Pools[p.ID], address, port) })
		if ips := cdnkit.UsableIPsAt(p, filter(personal), port); len(ips) > 0 {
			return ips
		}
	}
	originalKeep := keep
	keep = func(a string) bool {
		if originalKeep != nil && !originalKeep(a) {
			return false
		}
		return len(audience) == 0 || personalCDNAllowed(audience[0].Pools[p.ID], a, port)
	}
	// Apply exact-entry diagnostics to raw candidates before node filtering,
	// so a known failure still suppresses origin fallback when all nodes fail.
	reports, _ := s.cdnAudienceReports(p, port, carrier, cdnkit.AudienceRegion(audience))
	ips := s.cdnAddressesForPort(p, port)
	if len(reports) > 0 {
		ips = cdnkit.MergedReportIPs(p, reports)
	}
	ips = cdnkit.UsableIPsAt(p, filter(ips), port)
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
