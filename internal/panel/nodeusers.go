package panel

import (
	"encoding/json"
	"sync"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A node may only report traffic and online IPs for users it actually
// serves, so a compromised node cannot burn other users' quotas or get them
// IP-banned. The allowed set is the users in the node's desired and applied
// specs (both, so traffic counted just before a user is removed still lands).

type nodeUsersEntry struct {
	desired, applied int64
	set              map[string]bool
}

type nodeUsersCache struct {
	mu sync.Mutex
	m  map[string]nodeUsersEntry
}

func (s *Server) nodeUsers(nodeID string) map[string]bool {
	var n store.Node
	if s.st.DB.Select("id", "desired_rev", "applied_rev").First(&n, "id = ?", nodeID).Error != nil {
		return nil
	}
	s.nodeUserSets.mu.Lock()
	defer s.nodeUserSets.mu.Unlock()
	if s.nodeUserSets.m == nil {
		s.nodeUserSets.m = map[string]nodeUsersEntry{}
	}
	if e, ok := s.nodeUserSets.m[nodeID]; ok && e.desired == n.DesiredRev && e.applied == n.AppliedRev {
		return e.set
	}
	set := map[string]bool{}
	var revs []store.ConfigRevision
	s.st.DB.Where("node_id = ? AND rev IN ?", nodeID, []int64{n.DesiredRev, n.AppliedRev}).Find(&revs)
	for _, r := range revs {
		raw, err := s.st.Box.Open(r.Spec)
		if err != nil {
			continue
		}
		var spec model.NodeSpec
		if json.Unmarshal([]byte(raw), &spec) != nil {
			continue
		}
		for _, in := range spec.Inbounds {
			for _, u := range in.Users {
				set[u.ID] = true
			}
		}
	}
	s.nodeUserSets.m[nodeID] = nodeUsersEntry{n.DesiredRev, n.AppliedRev, set}
	return set
}
