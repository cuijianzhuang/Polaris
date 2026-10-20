package panel

import (
	"context"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
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
	// A webhook delivery cut off when the previous leader stopped is sent
	// again.
	s.st.DB.Model(&store.WebhookDelivery{}).Where("status = ?", "sending").Update("status", "pending")
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
			return
		case <-t.C:
			s.updateLease()
		}
	}
}
