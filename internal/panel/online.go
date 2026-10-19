package panel

import (
	"sort"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

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
	uids := make([]string, len(us))
	for i, u := range us {
		uids[i] = u.ID
	}
	snap := s.shared.onlineIPs(uids)
	now := time.Now()
	for _, u := range us {
		for node, ips := range snap[u.ID] {
			for ip, at := range ips {
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
		list := make([]store.OnlineDevice, 0, len(live[us[i].ID]))
		for ip, e := range live[us[i].ID] {
			d := store.OnlineDevice{IP: ip, Seen: e.at}
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
	}
}
