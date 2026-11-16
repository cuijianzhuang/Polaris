package panel

import (
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func normalizeOnlineIP(raw string) string {
	raw = strings.TrimSpace(raw)
	addr, err := netip.ParseAddr(strings.Trim(raw, "[]"))
	if err != nil {
		ap, e := netip.ParseAddrPort(raw)
		if e != nil {
			return ""
		}
		addr = ap.Addr()
	}
	addr = addr.Unmap().WithZone("")
	if addr.IsUnspecified() || addr.IsMulticast() {
		return ""
	}
	return addr.String()
}

func normalizedOnlineIPs(raw []string) []string {
	seen := map[string]bool{}
	ips := []string{}
	for _, value := range raw {
		if ip := normalizeOnlineIP(value); ip != "" && !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}
	sort.Strings(ips)
	return ips
}

// withOnline fills what is connected right now. The IPs come from the live
// reports in memory, so a user who disconnected stops showing IPs within
// onlineIPWindow instead of keeping the last stored list.
func (s *Server) withOnline(us []store.User) {
	if len(us) == 0 {
		return
	}
	type seen struct {
		nodes map[string]bool
		at    time.Time
	}
	live := map[string]map[string]*seen{} // user -> ip -> where and when
	last := map[string]time.Time{}
	uids := make([]string, len(us))
	for i, u := range us {
		uids[i] = u.ID
	}
	snap := s.shared.onlineIPs(uids)
	now := time.Now()
	for _, u := range us {
		for node, ips := range snap[u.ID] {
			for raw, at := range ips {
				ip := normalizeOnlineIP(raw)
				if ip == "" {
					continue
				}
				if at.After(last[u.ID]) {
					last[u.ID] = at
				}
				if now.Sub(at) >= onlineIPWindow {
					continue
				}
				if live[u.ID] == nil {
					live[u.ID] = map[string]*seen{}
				}
				e := live[u.ID][ip]
				if e == nil {
					e = &seen{nodes: map[string]bool{}}
					live[u.ID][ip] = e
				}
				e.nodes[node] = true
				if at.After(e.at) {
					e.at = at
				}
			}
		}
	}
	trusted := parseNetList(s.st.GetSetting("ip_limit_trusted", ""))

	var ids []string
	for id := range live {
		ids = append(ids, id)
	}
	names := map[string]string{}
	devices := map[string]store.UserDevice{} // user|ip -> newest client that fetched the subscription from it
	// A source address that is one of our own nodes is a relay hop or a CDN
	// origin fetch, not a client: count it, but never as the user's device.
	var nodeIPs netList
	if len(ids) > 0 {
		var ns []store.Node
		s.st.DB.Select("id", "name", "address", "public_ipv4", "public_ipv6").Find(&ns)
		for _, n := range ns {
			names[n.ID] = n.Name
		}
		nodeIPs = nodeOwnAddresses(ns)
		var ds []store.UserDevice
		s.st.DB.Where("user_id IN ?", ids).Order("last_seen").Find(&ds)
		for _, d := range ds {
			devices[d.UserID+"|"+d.IP] = d // later rows (newer) win
		}
	}
	for i := range us {
		u := &us[i]
		u.OnlineIPState, u.OnlineIPSeenAt, u.OnlineIPCount = "none", nil, nil
		if at := last[u.ID]; !at.IsZero() {
			u.OnlineIPSeenAt = &at
		}
		if u.OnlineAt != nil && now.Sub(*u.OnlineAt) < 2*time.Minute {
			u.OnlineIPState = "unavailable"
			if !last[u.ID].IsZero() {
				u.OnlineIPState = "expired"
			}
		}
		known, counted, proxy := 0, 0, 0
		list := make([]store.OnlineDevice, 0, len(live[us[i].ID]))
		for ip, e := range live[us[i].ID] {
			d := store.OnlineDevice{IP: ip, Seen: e.at}
			if cf, _ := cloudflareOwned(ip); cf {
				d.Excluded = "cdn"
				proxy++
			} else if nodeIPs.contains(ip) {
				// Our own node or relay hop, not the subscriber's device: a
				// chained inbound reports the previous hop's address.
				d.Excluded = "node"
				proxy++
			} else {
				known++
				if trusted.contains(ip) {
					d.Excluded = "trusted"
				} else {
					counted++
				}
			}
			for id := range e.nodes {
				if n := names[id]; n != "" {
					d.Nodes = append(d.Nodes, n)
				}
			}
			sort.Strings(d.Nodes)
			if dev, ok := devices[us[i].ID+"|"+ip]; ok {
				d.Model, d.OS = dev.Model, dev.OS
			}
			d.Geo, d.Network, d.GeoState = s.describeIP(ip)
			list = append(list, d)
		}
		sort.Slice(list, func(a, b int) bool { return list[a].IP < list[b].IP })
		ips := make([]string, len(list))
		for j, d := range list {
			ips[j] = d.IP
		}
		us[i].OnlineDevices = list
		us[i].OnlineIPs = store.NewJSON(ips)
		if len(list) > 0 {
			if u.OnlineAt == nil || u.OnlineAt.Before(last[u.ID]) {
				at := last[u.ID]
				u.OnlineAt = &at
			}
			u.OnlineIPState = "unavailable"
			if known > 0 {
				u.OnlineIPState = "available"
				if proxy > 0 {
					u.OnlineIPState = "partial"
				}
				u.OnlineIPCount = &counted
			}
		}
	}
}

// nodeOwnAddresses returns the addresses that belong to our own nodes: the
// configured public address plus whatever each agent reports as its public
// IPv4/IPv6. A source address matching one of them is a relay hop, a CDN
// origin fetch or an inbound bridged from another node, never a subscriber's
// device, so ip_limit and the online-IP list skip it.
func nodeOwnAddresses(nodes []store.Node) netList {
	var out netList
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		// The address may carry a port, and a hostname is resolved by the
		// caller's DNS: only literals are collected here, which is what the
		// agents report as their public address.
		if a, err := netip.ParseAddr(strings.Trim(raw, "[]")); err == nil {
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			return
		}
		if p, err := netip.ParseAddrPort(raw); err == nil {
			a := p.Addr().Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	for _, n := range nodes {
		add(n.Address)
		add(n.PublicIPv4)
		add(n.PublicIPv6)
	}
	return out
}

// nodeAddrSetCache holds the set of our own nodes' addresses. A node's public
// address changes rarely, and the online-IP path runs on every agent report,
// so a short TTL keeps a full node scan out of it.
type nodeAddrSetCache struct {
	mu      sync.Mutex
	set     netList
	builtAt time.Time
}

const nodeAddrSetTTL = 30 * time.Second

// ownNodeAddresses returns the cached set of this installation's node
// addresses, rebuilding it at most every nodeAddrSetTTL.
func (s *Server) ownNodeAddresses() netList {
	c := &s.nodeAddrCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set != nil && time.Since(c.builtAt) < nodeAddrSetTTL {
		return c.set
	}
	var ns []store.Node
	s.st.DB.Select("id", "address", "public_ipv4", "public_ipv6").Find(&ns)
	c.set, c.builtAt = nodeOwnAddresses(ns), time.Now()
	return c.set
}

// isOwnNodeAddress reports whether an address belongs to one of our nodes.
func (s *Server) isOwnNodeAddress(ip string) bool {
	return s.ownNodeAddresses().contains(ip)
}

// describeIP returns a human-readable location, the carrier or organisation
// and a state explaining an empty location. Both databases are optional: a
// panel without them still lists the addresses, just without the labels.
func (s *Server) describeIP(ip string) (geo, network, state string) {
	p, err := readRouteGeoPolicy(s.st.DB)
	if err != nil || !p.Enabled {
		state = "unavailable"
	} else if db, _ := s.loadRouteGeo(p.SHA256); db == nil {
		state = "unavailable"
	} else if m := db.Lookup(ip); m.State != "matched" {
		state = m.State
	} else {
		geo = geoText(m.Country, m.Province, m.City, m.CountryEN, m.ProvinceEN, m.CityEN)
		state = "matched"
		// m.Network is the CIDR the address matched, not an operator name:
		// the carrier comes from the ASN database below.
	}
	// The ASN database names the operator. The city database carries no
	// organisation, so this is the only source for it.
	if network == "" {
		if ap, err := readRouteASNPolicy(s.st.DB); err == nil && ap.Enabled {
			if adb, _ := s.loadRouteASN(ap.SHA256); adb != nil {
				if am := adb.Lookup(ip); am.State == "matched" {
					network = am.Organization
				}
			}
		}
	}
	return geo, network, state
}

// geoText joins the non-empty location parts, skipping repeats such as a city
// that equals its province.
func geoText(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" && (len(out) == 0 || out[len(out)-1] != p) {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}
