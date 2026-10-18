package hysteria

import (
	"context"
	"net"
	"sort"
	"time"
)

// maxClientsPerUser bounds what is remembered per user; a user with more
// simultaneous connections than this is far past any IP limit.
const maxClientsPerUser = 64

type client struct {
	addr string
	at   time.Time
}

// noteClient remembers the address a user authenticated from. Hysteria
// calls the auth backend once per client connection and says where from.
func (a *Adapter) noteClient(user, addr string) {
	if user == "" || addr == "" {
		return
	}
	a.cmu.Lock()
	defer a.cmu.Unlock()
	if a.clients == nil {
		a.clients = map[string][]client{}
	}
	list := a.clients[user]
	for i := range list {
		if list[i].addr == addr {
			list[i].at = time.Now()
			a.clients[user] = list
			return
		}
	}
	list = append(list, client{addr: addr, at: time.Now()})
	if len(list) > maxClientsPerUser {
		sort.Slice(list, func(i, j int) bool { return list[i].at.After(list[j].at) })
		list = list[:maxClientsPerUser]
	}
	a.clients[user] = list
}

// OnlineIPs lists the source IPs of connected clients. The stats API says how
// many connections each user has but not from where, so the addresses seen at
// authentication fill that in: the newest N of them for a user with N
// connections (a closed connection is not announced, the count is).
func (a *Adapter) OnlineIPs(ctx context.Context) (map[string][]string, error) {
	a.mu.RLock()
	var apis []string
	for _, in := range a.insts {
		apis = append(apis, in.api)
	}
	a.mu.RUnlock()
	counts := map[string]int{}
	var lastErr error
	ok := false
	for _, api := range apis {
		var m map[string]int
		if err := a.get(ctx, api, "/online", &m); err != nil {
			lastErr = err
			continue
		}
		ok = true
		for u, n := range m {
			counts[u] += n
		}
	}
	if !ok && lastErr != nil {
		return nil, lastErr
	}
	out := map[string][]string{}
	a.cmu.Lock()
	defer a.cmu.Unlock()
	for u := range a.clients {
		if counts[u] == 0 {
			delete(a.clients, u) // nobody is connected any more
		}
	}
	for u, n := range counts {
		list := append([]client(nil), a.clients[u]...)
		sort.Slice(list, func(i, j int) bool { return list[i].at.After(list[j].at) })
		if len(list) > n {
			list = list[:n]
		}
		seen := map[string]bool{}
		for _, c := range list {
			host, _, err := net.SplitHostPort(c.addr)
			if err != nil {
				host = c.addr
			}
			if !seen[host] {
				seen[host] = true
				out[u] = append(out[u], host)
			}
		}
	}
	return out, nil
}
