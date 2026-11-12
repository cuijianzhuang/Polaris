package panel

import (
	"slices"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/panel/cdnkit"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// subPrefetch is the data one subscription render needs from the database,
// fetched once up front and shared by every inbound/endpoint expansion.
// Without it, rendering N inbounds issued O(N) endpoint, CDN profile,
// fronting and CDN report queries; with it, the same render issues a
// constant number of batched queries plus one cached report fetch per
// distinct (profile, port, carrier, region).
//
// It is per-request state: a new one is built for every render and never
// shared between requests, so the CDN report cache cannot serve stale
// data to another user.
type subPrefetch struct {
	s *Server
	// endpoints by inbound ID, in sort_order, created_at order — the same
	// order the old per-inbound query returned.
	endpoints map[string][]store.Endpoint
	profiles  map[string]store.CDNProfile
	fronts    map[string]frontResult
	// inboundsByNode lists every enabled inbound on the rendered nodes,
	// used to evaluate Caddy fronting blockers without extra queries.
	inboundsByNode map[string][]store.Inbound
	// Report cutoff per profile, fixed at build time so every cached
	// report fetch in this render uses the same instant.
	cutoffs map[string]time.Time
	// CDN report cache, keyed by the exact inputs of cdnkit.ReportsForPort.
	reports map[cdnReportKey][]store.CDNIPReport
	// cdnAddressesForPort results, keyed the same way (carrier/region "").
	addresses map[cdnReportKey][]string
}

type frontResult struct {
	info frontInfo
	ok   bool
}

type cdnReportKey struct {
	profileID string
	port      int
	carrier   string
	region    string
}

// buildSubPrefetch fetches, in a constant number of queries, everything
// subNodesForTrace needs for the given inbounds: their endpoints, the CDN
// profiles those endpoints reference, and the inbounds on the same nodes
// that Caddy fronting blockers are evaluated against. all is every enabled
// inbound on the rendered nodes (from fallbackMastersFor).
func (s *Server) buildSubPrefetch(ins []store.Inbound, all []store.Inbound) *subPrefetch {
	pre := &subPrefetch{
		s:              s,
		endpoints:      map[string][]store.Endpoint{},
		profiles:       map[string]store.CDNProfile{},
		fronts:         map[string]frontResult{},
		inboundsByNode: map[string][]store.Inbound{},
		cutoffs:        map[string]time.Time{},
		reports:        map[cdnReportKey][]store.CDNIPReport{},
		addresses:      map[cdnReportKey][]string{},
	}
	if len(ins) == 0 {
		return pre
	}
	// Endpoints for every enabled inbound on the rendered nodes (the
	// all list passed in), so fronting evaluation for blocker inbounds
	// needs no further queries. The rendered inbounds are a subset of
	// that set.
	ids := make([]string, 0, len(all)+len(ins))
	seen := map[string]bool{}
	for _, in := range all {
		if !seen[in.ID] {
			seen[in.ID] = true
			ids = append(ids, in.ID)
		}
		pre.inboundsByNode[in.NodeID] = append(pre.inboundsByNode[in.NodeID], in)
	}
	for _, in := range ins {
		if !seen[in.ID] {
			seen[in.ID] = true
			ids = append(ids, in.ID)
		}
	}
	var eps []store.Endpoint
	s.st.DB.Where("inbound_id IN ?", ids).Order("sort_order, created_at").Find(&eps)
	profileIDs := make([]string, 0)
	profileSeen := map[string]bool{}
	for _, ep := range eps {
		pre.endpoints[ep.InboundID] = append(pre.endpoints[ep.InboundID], ep)
		if ep.Kind == "cdn" && ep.CDNProfileID != "" && !profileSeen[ep.CDNProfileID] {
			profileSeen[ep.CDNProfileID] = true
			profileIDs = append(profileIDs, ep.CDNProfileID)
		}
	}
	if len(profileIDs) > 0 {
		var ps []store.CDNProfile
		s.st.DB.Where("id IN ?", profileIDs).Find(&ps)
		for _, p := range ps {
			pre.profiles[p.ID] = p
			pre.cutoffs[p.ID] = cdnkit.SelectionCutoff(&p)
		}
	}
	for _, in := range ins {
		pre.fronts[in.ID] = pre.frontFor(&in)
	}
	return pre
}

// front returns the Caddy fronting of an inbound, computed from the
// prefetched endpoints/profiles/inbounds. It mirrors Server.frontFor,
// including the blocker check, without touching the database.
func (pre *subPrefetch) front(in *store.Inbound) (frontInfo, bool) {
	return pre.frontByID(in.ID)
}

func (pre *subPrefetch) frontByID(inboundID string) (frontInfo, bool) {
	r := pre.fronts[inboundID]
	return r.info, r.ok
}

func (pre *subPrefetch) frontFor(in *store.Inbound) frontResult {
	fi, ok := pre.frontProfile(in.ID)
	if !ok {
		return frontResult{}
	}
	if pre.frontBlocker(in, fi.Port) != "" {
		return frontResult{}
	}
	return frontResult{info: fi, ok: true}
}

// frontProfile mirrors frontProfile in fronting.go, reading endpoints and
// profiles from the prefetch instead of the database. The prefetched
// endpoints are in sort_order order; the old query had no ORDER BY, so
// its first match was unspecified — sort order is the deterministic
// choice within that same unspecified space.
func (pre *subPrefetch) frontProfile(inboundID string) (frontInfo, bool) {
	for _, ep := range pre.endpoints[inboundID] {
		if ep.Kind != "cdn" {
			continue
		}
		p, ok := pre.profiles[ep.CDNProfileID]
		if !ok || !p.CaddyFronting || cdnkit.Scheme(&p) == "http" {
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

// frontBlocker mirrors Server.frontBlockerOn, evaluated against the
// prefetched inbounds of the node.
func (pre *subPrefetch) frontBlocker(in *store.Inbound, port int) string {
	for _, row := range pre.inboundsByNode[in.NodeID] {
		if row.Port != port || !row.Enabled || row.ID == in.ID {
			continue
		}
		m := pre.s.toModelInbound(&row)
		if !m.UsesTCP() {
			continue
		}
		if _, fronted := pre.frontProfile(row.ID); fronted && m.CDNCapable() && m.Security.Type == model.SecTLS {
			continue
		}
		return row.Tag
	}
	return ""
}

// reportsForPort mirrors cdnkit.ReportsForPort, cached per (profile, port,
// carrier, region) for the lifetime of this render.
func (pre *subPrefetch) reportsForPort(p *store.CDNProfile, port int, carrier, region string) []store.CDNIPReport {
	key := cdnReportKey{profileID: p.ID, port: port, carrier: carrier, region: region}
	if rs, ok := pre.reports[key]; ok {
		return rs
	}
	var regions []string
	if region != "" {
		regions = []string{region}
	}
	rs, _ := cdnkit.ReportsForPort(dbStore{pre.s.st.DB}, p, port, pre.cutoffs[p.ID], carrier, regions...)
	pre.reports[key] = rs
	return rs
}

// audienceReports mirrors cdnkit.AudienceReports, reading reports from
// the per-render cache instead of the database.
func (pre *subPrefetch) audienceReports(p *store.CDNProfile, port int, carrier, region string) ([]store.CDNIPReport, string) {
	if p.RegionSplit && region != "" {
		filterCarrier := ""
		if p.CarrierSplit {
			filterCarrier = carrier
		}
		reports := pre.reportsForPort(p, port, filterCarrier, region)
		if len(reports) > 0 {
			if filterCarrier != "" {
				return reports, "region_carrier"
			}
			return reports, "region"
		}
	}
	if p.CarrierSplit && carrier != "" {
		reports := pre.reportsForPort(p, port, carrier, "")
		if len(reports) > 0 {
			return reports, "carrier"
		}
	}
	return nil, "merged"
}

// addressesForPort mirrors Server.cdnAddressesForPort, reading reports
// from the per-render cache instead of the database.
func (pre *subPrefetch) addressesForPort(p *store.CDNProfile, port int) []string {
	key := cdnReportKey{profileID: p.ID, port: port}
	if list, ok := pre.addresses[key]; ok {
		return list
	}
	var list []string
	if port == cdnkit.DefaultPort(p) {
		list = p.PreferredIPs.V
	} else {
		list = p.PreferredIPs.V
		if reports := pre.reportsForPort(p, port, "", ""); len(reports) > 0 {
			list = cdnkit.MergedReportIPs(p, reports)
		}
	}
	pre.addresses[key] = list
	return list
}

// cdnIPsForPort mirrors Server.cdnIPsForPort on the cached addresses.
func (pre *subPrefetch) cdnIPsForPort(p *store.CDNProfile, port int) []string {
	return cdnkit.UsableIPsAt(p, pre.addressesForPort(p, port), port)
}

// selectedCDNIPs mirrors Server.selectedCDNIPsWhere, except the report
// and address lookups come from the per-render cache. The personal
// audience branch, keep filtering and top-N/pin handling are unchanged.
func (pre *subPrefetch) selectedCDNIPs(p *store.CDNProfile, port int, carrier string, keep func(string) bool, audience ...cdnkit.Audience) []string {
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
	reports, _ := pre.audienceReports(p, port, carrier, cdnkit.AudienceRegion(audience))
	ips := pre.addressesForPort(p, port)
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

// cdnTarget mirrors Server.cdnTargetFor, except the fronting lookup and
// the preferred-IP lookup come from the prefetch instead of the database.
func (pre *subPrefetch) cdnTarget(ep *store.Endpoint, m *model.Inbound, p *store.CDNProfile) cdnkit.Target {
	front, fronted := pre.frontByID(m.ID)
	host := orDefault(ep.Host, p.OriginDomain)
	t := cdnkit.Target{Provider: p.Provider, OriginPort: m.Port, Scheme: cdnkit.Scheme(p), Server: host, Port: cdnkit.NodePort(p, m.Port, fronted && p.CaddyFronting), SNI: host, Host: host, Path: m.Transport.Path}
	if m.Transport.Type == model.TransGRPC {
		t.Path = "/" + m.Transport.ServiceName
	}
	if ep.Address != "" {
		t.Server = ep.Address
	}
	if ep.Port > 0 {
		t.Port = ep.Port
	}
	if fronted && p.CaddyFronting {
		t.OriginPort = front.Port
	}
	if p.ExpandPerIP && ep.Address == "" {
		if ips := pre.cdnIPsForPort(p, t.Port); len(ips) > 0 {
			t.Server = ips[0]
		}
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
