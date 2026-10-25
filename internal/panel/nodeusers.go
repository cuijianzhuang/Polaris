package panel

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A node may only report traffic and online IPs for users it actually
// serves, so a compromised node cannot burn other users' quotas or get them
// IP-banned. The allowed set is the users in the node's desired and applied
// specs (both, so traffic counted just before a user is removed still lands).

type nodeUsersEntry struct {
	desired, applied int64
	set              map[string]bool // from the two revisions
	own              map[string]bool // set without grace users
	graceUntil       time.Time
}

// nodeUsersGrace keeps the users of the previously applied revision allowed
// for a while: the agent reports their last traffic after it has applied a
// spec without them (and may skip revisions, so "previous" is not rev-1).
const nodeUsersGrace = 5 * time.Minute

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
	prev, hadPrev := s.nodeUserSets.m[nodeID]
	if hadPrev && prev.desired == n.DesiredRev && prev.applied == n.AppliedRev {
		if !prev.graceUntil.IsZero() && time.Now().After(prev.graceUntil) {
			prev.set, prev.graceUntil = prev.own, time.Time{}
			s.nodeUserSets.m[nodeID] = prev
		}
		return prev.set
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
	e := nodeUsersEntry{desired: n.DesiredRev, applied: n.AppliedRev, own: set, set: set}
	// Callers read the returned map without the lock: build a new one.
	union := func(a, b map[string]bool) map[string]bool {
		m := make(map[string]bool, len(a)+len(b))
		for id := range a {
			m[id] = true
		}
		for id := range b {
			m[id] = true
		}
		return m
	}
	switch {
	case hadPrev && prev.applied != n.AppliedRev:
		e.set, e.graceUntil = union(prev.own, set), time.Now().Add(nodeUsersGrace)
	case hadPrev && !prev.graceUntil.IsZero() && time.Now().Before(prev.graceUntil):
		e.set, e.graceUntil = union(prev.set, set), prev.graceUntil // only desired moved: keep the grace
	}
	s.nodeUserSets.m[nodeID] = e
	return e.set
}
