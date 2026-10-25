package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
	"gorm.io/gorm/clause"
)

// Several panel instances may share one PostgreSQL database. One of them,
// the leader, holds a lease in the database and runs everything that must
// happen once: scheduled jobs and Telegram polling. The others serve the
// admin UI, subscriptions and agents. When the leader stops renewing, another
// instance takes over within leaseTTL.

const (
	leaseName  = "leader"
	leaseTTL   = 15 * time.Second
	leaseRenew = 5 * time.Second
)

// isLeader reports whether this instance runs the cluster-wide work. A
// SQLite panel is always alone, so always the leader.
func (s *Server) isLeader() bool { return s.leader.Load() }

// tryLease takes or renews the leader lease and reports whether this
// instance holds it. Times come from the database, so instances with
// skewed clocks agree.
func (s *Server) tryLease() bool {
	db := s.st.DB
	if db.Dialector.Name() != "postgres" {
		return true
	}
	db.Exec("INSERT INTO cluster_leases (name, holder, expires_at) VALUES (?, '', now()) ON CONFLICT (name) DO NOTHING", leaseName)
	r := db.Exec("UPDATE cluster_leases SET holder = ?, expires_at = now() + make_interval(secs => ?) WHERE name = ? AND (holder = ? OR expires_at < now())",
		s.procID, leaseTTL.Seconds(), leaseName, s.procID)
	return r.Error == nil && r.RowsAffected == 1
}

// updateLease runs one lease round and logs leadership changes.
func (s *Server) updateLease() {
	now := s.tryLease()
	if was := s.leader.Swap(now); was != now {
		if now {
			s.log.Info("this instance is now the leader (runs scheduled jobs and the Telegram bot)", "instance", s.procID)
			s.onLeader()
		} else {
			s.log.Warn("leader lease lost; another instance runs scheduled jobs now", "instance", s.procID)
		}
	}
}

// onLeader runs once each time this instance becomes the leader.
func (s *Server) onLeader() {
	s.requeueStaleWebhooks()
}

// runLease keeps the lease renewed until ctx ends, then releases it so
// another instance takes over at once.
func (s *Server) runLease(ctx context.Context) {
	t := time.NewTicker(leaseRenew)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if s.isLeader() && s.st.DB.Dialector.Name() == "postgres" {
				s.st.DB.Exec("UPDATE cluster_leases SET expires_at = now() WHERE name = ? AND holder = ?", leaseName, s.procID)
			}
			s.leader.Store(false)
			s.memberLeave()
			return
		case <-t.C:
			s.updateLease()
			s.memberTouch()
		}
	}
}

// --- agents connected to other instances ---

// clusterHub implements hubRemote over the cluster bus and agent_presences.
type clusterHub struct {
	s   *Server
	bus *clusterBus

	mu      sync.Mutex
	waiting map[string]chan cmdResult // forwarded commands by id
	seenAt  time.Time                 // presence cache
	present map[string]bool           // nodes with a live stream on another instance
}

type specNotice struct {
	Node string `json:"node"`
	Rev  int64  `json:"rev"`
}

type cmdRequest struct {
	ID      string            `json:"id"`
	Node    string            `json:"node"`
	Kind    string            `json:"kind"`
	Args    map[string]string `json:"args,omitempty"`
	Timeout time.Duration     `json:"timeout"`
}

type cmdResult struct {
	ID     string          `json:"id"`
	To     string          `json:"to"`
	Result json.RawMessage `json:"result,omitempty"` // protojson CommandResult
	Error  string          `json:"error,omitempty"`
}

// presenceTTL: an instance that stops refreshing a presence row (crash) no
// longer counts as holding the node after this long. Agents send heartbeats
// every few seconds; rows are refreshed every presenceTouch.
const (
	presenceTTL   = 45 * time.Second
	presenceTouch = 15 * time.Second
)

func newClusterHub(s *Server, bus *clusterBus) *clusterHub {
	c := &clusterHub{s: s, bus: bus, waiting: map[string]chan cmdResult{}}
	bus.subscribe("spec", func(_ string, data []byte) {
		var n specNotice
		if json.Unmarshal(data, &n) == nil && s.hub.Local(n.Node) {
			s.hub.notifyLocal(n.Node, n.Rev)
		}
	})
	bus.subscribe("kick", func(from string, data []byte) {
		var node string
		if from != s.procID && json.Unmarshal(data, &node) == nil {
			s.hub.mu.Lock()
			if conn, ok := s.hub.conns[node]; ok {
				delete(s.hub.conns, node)
				close(conn.done)
			}
			s.hub.mu.Unlock()
		}
	})
	bus.subscribe("cmd", func(from string, data []byte) {
		var r cmdRequest
		if json.Unmarshal(data, &r) != nil || !s.hub.Local(r.Node) {
			return
		}
		res := cmdResult{ID: r.ID, To: from}
		out, err := s.hub.commandLocal(context.Background(), r.Node, r.Kind, r.Args, r.Timeout)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Result, _ = protojson.Marshal(out)
		}
		_ = bus.publish("cmd-result", res)
	})
	bus.subscribe("cmd-result", func(_ string, data []byte) {
		var r cmdResult
		if json.Unmarshal(data, &r) != nil || r.To != s.procID {
			return
		}
		c.mu.Lock()
		ch := c.waiting[r.ID]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- r:
			default:
			}
		}
	})
	return c
}

func (c *clusterHub) online(nodeID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.seenAt) > 2*time.Second {
		var rows []store.AgentPresence
		c.s.st.DB.Where("instance_id <> ? AND seen_at > ?", c.s.procID, time.Now().Add(-presenceTTL)).Find(&rows)
		c.present = map[string]bool{}
		for _, r := range rows {
			c.present[r.NodeID] = true
		}
		c.seenAt = time.Now()
	}
	return c.present[nodeID]
}

func (c *clusterHub) notifySpec(nodeID string, rev int64) {
	_ = c.bus.publish("spec", specNotice{Node: nodeID, Rev: rev})
}

func (c *clusterHub) kick(nodeID string) {
	_ = c.bus.publish("kick", nodeID)
}

func (c *clusterHub) command(ctx context.Context, nodeID, kind string, args map[string]string, timeout time.Duration) (*polarisv1.CommandResult, error) {
	id := store.NewID()
	ch := make(chan cmdResult, 1)
	c.mu.Lock()
	c.waiting[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
	}()
	if err := c.bus.publish("cmd", cmdRequest{ID: id, Node: nodeID, Kind: kind, Args: args, Timeout: timeout}); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second) // the holder waits timeout for the agent
	defer cancel()
	select {
	case r := <-ch:
		if r.Error != "" {
			return nil, errors.New(r.Error)
		}
		var out polarisv1.CommandResult
		if err := protojson.Unmarshal(r.Result, &out); err != nil {
			return nil, err
		}
		return &out, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("agent did not answer: %w", ctx.Err())
	}
}

// presenceUp records that this instance holds the node's stream.
func (s *Server) presenceUp(nodeID string) {
	if s.hub.remote == nil {
		return
	}
	s.st.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&store.AgentPresence{NodeID: nodeID, InstanceID: s.procID, SeenAt: time.Now()})
}

// presenceTouch refreshes the row on heartbeats, at most every presenceTouch.
func (s *Server) presenceTouch(nodeID string) {
	if s.hub.remote == nil {
		return
	}
	s.presMu.Lock()
	due := time.Since(s.presAt[nodeID]) >= presenceTouch
	if due {
		s.presAt[nodeID] = time.Now()
	}
	s.presMu.Unlock()
	if due {
		s.st.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&store.AgentPresence{NodeID: nodeID, InstanceID: s.procID, SeenAt: time.Now()})
	}
}

// presenceDown drops this instance's row once the node's stream is gone
// from it (a newer stream on another instance keeps its own row).
func (s *Server) presenceDown(nodeID string) {
	if s.hub.remote == nil || s.hub.Local(nodeID) {
		return
	}
	s.presMu.Lock()
	delete(s.presAt, nodeID)
	s.presMu.Unlock()
	s.st.DB.Where("node_id = ? AND instance_id = ?", nodeID, s.procID).Delete(&store.AgentPresence{})
}

// shareNodeChanges tells the other instances, at most every 2 seconds, that
// the node list changed here (their admin pages refresh from the database).
func (s *Server) shareNodeChanges(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.rtShare.Swap(false) {
				_ = s.bus.publish("nodes", nil)
			}
		}
	}
}

// shareHeartbeatState stores what only the instance holding a node's stream
// knows (install progress, users under a speed limit) for the others, at most
// every 5 seconds per node unless force (a progress step finished or failed).
func (s *Server) shareHeartbeatState(nodeID, kind string, v any, force bool) {
	if s.hub.remote == nil {
		return
	}
	k := kind + ":" + nodeID
	s.presMu.Lock()
	due := force || time.Since(s.shareAt[k]) >= 5*time.Second
	if due {
		s.shareAt[k] = time.Now()
	}
	s.presMu.Unlock()
	if due {
		_ = s.eph.put("hb:"+k, v, 15*time.Minute)
	}
}

// remoteHeartbeatState reads what another instance shared for a node.
func (s *Server) remoteHeartbeatState(nodeID, kind string, v any) bool {
	if s.hub.remote == nil || s.hub.Local(nodeID) {
		return false
	}
	return s.eph.get("hb:"+kind+":"+nodeID, v)
}
