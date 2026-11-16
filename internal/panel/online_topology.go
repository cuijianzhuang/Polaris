package panel

import (
	"sort"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Topology is configuration, not flow correlation. In particular, a client
// observed at A does not become a relay address merely because A forwards to B.
type onlineTopology struct {
	targets       map[string][]store.OnlineNodeRef
	addressOwners map[string][]store.OnlineNodeRef
}

func buildOnlineTopology(nodes []store.Node, chains []store.Chain) onlineTopology {
	out := onlineTopology{targets: map[string][]store.OnlineNodeRef{}, addressOwners: map[string][]store.OnlineNodeRef{}}
	names := map[string]string{}
	edges := map[string]map[string]bool{}
	owners := map[string]map[string]bool{}
	for _, n := range nodes {
		names[n.ID] = n.Name
	}
	add := func(from, to string) {
		if from == to || names[from] == "" || names[to] == "" {
			return
		}
		if edges[from] == nil {
			edges[from] = map[string]bool{}
		}
		edges[from][to] = true
	}
	for _, n := range nodes {
		for _, to := range n.RelayFor.V {
			add(n.ID, to)
		}
		for _, raw := range []string{n.Address, n.PublicIPv4, n.PublicIPv6} {
			ip := normalizeOnlineIP(raw)
			if ip == "" {
				continue
			}
			if owners[ip] == nil {
				owners[ip] = map[string]bool{}
			}
			owners[ip][n.ID] = true
		}
	}
	for _, ch := range chains {
		if ch.Disabled {
			continue
		}
		for i := 1; i < len(ch.Hops.V); i++ {
			add(ch.Hops.V[i-1].NodeID, ch.Hops.V[i].NodeID)
		}
	}
	refs := func(ids map[string]bool) []store.OnlineNodeRef {
		list := make([]store.OnlineNodeRef, 0, len(ids))
		for id := range ids {
			list = append(list, store.OnlineNodeRef{ID: id, Name: names[id]})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
		return list
	}
	for id, targets := range edges {
		out.targets[id] = refs(targets)
	}
	for ip, ids := range owners {
		out.addressOwners[ip] = refs(ids)
	}
	return out
}
