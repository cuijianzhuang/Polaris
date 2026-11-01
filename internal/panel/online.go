package panel

import (
	"net/netip"
	"sort"
	"strings"
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
	if len(ids) > 0 {
		var ns []store.Node
		s.st.DB.Select("id", "name").Find(&ns)
		for _, n := range ns {
			names[n.ID] = n.Name
		}
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
