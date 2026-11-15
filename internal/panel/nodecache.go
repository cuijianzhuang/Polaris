package panel

import (
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// These maps only throttle writes (5/15/30 seconds); an idle entry older than
// five minutes cannot suppress a write, so removing it has no semantic effect.
const nodeThrottleRetention = 5 * time.Minute

// pruneNodeCaches runs on each instance: these caches are process-local, not
// covered by the cluster leader's database cleanup. Never take cache locks
// while holding a DB transaction: nodeUsers performs DB reads under its mutex.
func (s *Server) pruneNodeCaches() {
	now := time.Now()
	s.presMu.Lock()
	for _, cache := range []map[string]time.Time{s.presAt, s.specAt, s.shareAt} {
		for key, at := range cache {
			if now.Sub(at) > nodeThrottleRetention {
				delete(cache, key)
			}
		}
	}
	s.presMu.Unlock()

	// Existing nodes keep their revision history and five-minute grace set,
	// even while disconnected. Only confirmed missing nodes are eligible.
	s.nodeUserSets.mu.Lock()
	generations := make(map[string]uint64, len(s.nodeUserSets.m))
	ids := make([]string, 0, len(s.nodeUserSets.m))
	for id, entry := range s.nodeUserSets.m {
		ids = append(ids, id)
		generations[id] = entry.generation
	}
	s.nodeUserSets.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	live := make(map[string]bool, len(ids))
	// Stay below SQLite's bind limit on panels with many historical nodes.
	const batch = 500
	for start := 0; start < len(ids); start += batch {
		var nodes []store.Node
		if err := s.st.DB.Select("id").Where("id IN ?", ids[start:min(start+batch, len(ids))]).Find(&nodes).Error; err != nil {
			s.log.Warn("node cache cleanup skipped", "err", err)
			return // no existence decision is safe after a failed lookup
		}
		for _, n := range nodes {
			live[n.ID] = true
		}
	}
	s.nodeUserSets.mu.Lock()
	defer s.nodeUserSets.mu.Unlock()
	for id, generation := range generations {
		if current, ok := s.nodeUserSets.m[id]; !live[id] && ok && current.generation == generation {
			delete(s.nodeUserSets.m, id)
		}
	}
	// A late handler may still insert a deleted node after this sweep. The
	// next sweep reclaims it; nodeUsers checks DB existence before use, so
	// such retained entries cannot authorize reports from a deleted node.
}
