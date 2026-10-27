package panel

import (
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Cluster-wide operations for panel instances sharing a database: who is
// running (cluster_members), restarting the others after a restore and
// updating every instance at once.

const (
	memberTouch      = 15 * time.Second
	memberOnline     = 45 * time.Second // seen within: running
	memberForget     = 24 * time.Hour   // rows older than this are removed
	topicRestoreDone = "restore-done"
	topicUpdate      = "update"
)

// subscribeClusterOps handles the bus topics of the other instances.
func (s *Server) subscribeClusterOps() {
	s.bus.subscribe(topicRestoreDone, func(from string, _ []byte) {
		if from == s.procID || s.hasClusterDelivery("restore", "") {
			return
		}
		s.log.Warn("another instance restored a backup; restarting to load it", "from", from)
		go func() {
			time.Sleep(time.Second)
			s.restartNow()
		}()
	})
	s.bus.subscribe(topicUpdate, func(from string, data []byte) {
		if from == s.procID {
			return
		}
		target := string(data)
		if len(target) >= 2 && target[0] == '"' {
			target = target[1 : len(target)-1]
		}
		if s.hasClusterDelivery("update", target) {
			return
		}
		if err := s.writeUpdateRequest(target); err != nil {
			s.log.Warn("panel update requested by another instance", "target", target, "err", err)
			return
		}
		s.log.Info("panel update requested by another instance", "target", target, "from", from)
	})
}

// memberTouch refreshes this instance's cluster_members row (at most every
// memberTouch); the leader also forgets instances gone for a day.
func (s *Server) memberTouch() {
	if time.Since(s.memberAt) < memberTouch {
		return
	}
	s.memberAt = time.Now()
	s.warnWrite("cluster member", s.st.DB.Where("member_id = ? AND instance_id <> ?", s.memberID, s.procID).Delete(&store.ClusterMember{}).Error)
	host, _ := os.Hostname()
	s.warnWrite("cluster member", s.st.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&store.ClusterMember{
		InstanceID: s.procID, MemberID: s.memberID, Host: host, Version: Version, Edition: Edition(), StartedAt: s.startedAt, SeenAt: time.Now(),
	}).Error)
	if s.isLeader() {
		s.warnWrite("cluster members", s.st.DB.Where("seen_at < ?", time.Now().Add(-memberForget)).Delete(&store.ClusterMember{}).Error)
	}
}

// memberLeave removes this instance's row on shutdown.
func (s *Server) memberLeave() {
	s.st.DB.Where("instance_id = ?", s.procID).Delete(&store.ClusterMember{})
}

type clusterMember struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Version   string    `json:"version"`
	Edition   string    `json:"edition"`
	StartedAt time.Time `json:"startedAt"`
	SeenAt    time.Time `json:"seenAt"`
	Online    bool      `json:"online"`
	Leader    bool      `json:"leader"`
	Self      bool      `json:"self"`
}

// clusterMembers lists the instances seen within a day, running ones first.
func (s *Server) clusterMembers() []clusterMember {
	var rows []store.ClusterMember
	s.st.DB.Order("started_at").Find(&rows)
	holder := s.procID
	if s.st.DB.Dialector.Name() == "postgres" {
		var l store.ClusterLease
		holder = ""
		if s.st.DB.Where("name = ? AND expires_at > now()", leaseName).First(&l).Error == nil {
			holder = l.Holder
		}
	}
	out := []clusterMember{}
	seenSelf := false
	for _, r := range rows {
		m := clusterMember{ID: r.InstanceID, Host: r.Host, Version: r.Version, Edition: r.Edition, StartedAt: r.StartedAt, SeenAt: r.SeenAt,
			Online: time.Since(r.SeenAt) < memberOnline, Self: r.InstanceID == s.procID}
		if m.Self {
			m.Online, seenSelf = true, true
		}
		m.Leader = m.Online && r.InstanceID == holder
		out = append(out, m)
	}
	if !seenSelf { // not written yet
		host, _ := os.Hostname()
		out = append(out, clusterMember{ID: s.procID, Host: host, Version: Version, Edition: Edition(), StartedAt: s.startedAt, SeenAt: time.Now(),
			Online: true, Leader: s.isLeader(), Self: true})
	}
	// Running instances first, in start order.
	sorted := make([]clusterMember, 0, len(out))
	for _, on := range []bool{true, false} {
		for _, m := range out {
			if m.Online == on {
				sorted = append(sorted, m)
			}
		}
	}
	return sorted
}

// hostTag prefixes messages with this server's name when several instances
// are running, so a Telegram report says which one it is about.
func (s *Server) hostTag() string {
	n := 0
	for _, m := range s.clusterMembers() {
		if m.Online {
			n++
		}
	}
	if n < 2 {
		return ""
	}
	host, _ := os.Hostname()
	return "[" + host + "] "
}

// writeUpdateRequest asks this machine's root updater to install target.
func (s *Server) writeUpdateRequest(target string) error {
	if Version == "dev" {
		return errDevUpdate
	}
	if !s.updaterInstalled() {
		return errUpdaterMissing
	}
	if _, err := os.Stat(s.updateRequestPath()); err == nil {
		return errUpdatePending
	}
	if target == "" {
		target = "latest"
	}
	return core.AtomicWriteDurable(s.updateRequestPath(), []byte(target+"\n"), 0o644)
}

// getCluster is GET /cluster.
func (s *Server) getCluster(c *gin.Context) {
	var ops []store.ClusterOperation
	s.st.DB.Order("created_at desc").Limit(20).Find(&ops)
	var deliveries []store.ClusterDelivery
	var ids []string
	for _, op := range ops {
		ids = append(ids, op.ID)
	}
	if len(ids) > 0 {
		s.st.DB.Where("operation_id IN ?", ids).Find(&deliveries)
	}
	c.JSON(http.StatusOK, gin.H{"members": s.clusterMembers(), "shared": s.bus != nil, "operations": ops, "deliveries": deliveries})
}
