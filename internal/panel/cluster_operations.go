package panel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func loadMemberID(dir string) (string, error) {
	path := filepath.Join(dir, "cluster-instance-id")
	raw, e := os.ReadFile(path)
	if e == nil {
		id := strings.TrimSpace(string(raw))
		if len(id) != 16 || strings.Trim(id, "abcdefghijkmnopqrstuvwxyz23456789") != "" {
			return "", errors.New("invalid cluster installation identity")
		}
		return id, nil
	}
	if !errors.Is(e, os.ErrNotExist) {
		return "", e
	}
	id := store.NewID()
	return id, core.AtomicWriteDurable(path, []byte(id+"\n"), 0600)
}

func (s *Server) queueClusterOperation(kind, target string, includeSelf bool) (string, error) {
	now := time.Now().UTC()
	op := store.ClusterOperation{ID: store.NewID(), Kind: kind, Target: target, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	legacy := false
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var e error
		legacy, e = saveClusterOperation(tx, op, s.memberID, s.procID, includeSelf)
		return e
	})
	if err != nil {
		return "", err
	}
	if s.bus != nil {
		if e := s.bus.publish("cluster-ops", op.ID); e != nil {
			s.log.Warn("cluster operation queued; notification unavailable", "operation", op.ID)
		}
		// Rolling upgrades still reach old processes. They cannot acknowledge the
		// durable operation, so the dashboard keeps their targets visibly pending.
		if legacy {
			if kind == "update" {
				_ = s.bus.publish(topicUpdate, target)
			} else {
				_ = s.bus.publish(topicRestoreDone, s.procID)
			}
		}
	}
	return op.ID, nil
}

func (s *Server) runClusterOperations(ctx context.Context) {
	s.processClusterOperations()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.processClusterOperations()
		}
	}
}

func (s *Server) processClusterOperations() {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	var ds []store.ClusterDelivery
	now := time.Now().UTC()
	if e := s.st.DB.Where("member_id = ? AND status = ? AND next_at <= ?", s.memberID, "pending", now).Order("next_at").Limit(20).Find(&ds).Error; e != nil {
		return
	}
	for _, d := range ds {
		var op store.ClusterOperation
		if s.st.DB.First(&op, "id = ?", d.OperationID).Error != nil {
			continue
		}
		var err error
		switch {
		case now.After(op.ExpiresAt):
			err = errors.New("操作已超时，请检查实例后重新发起")
		case op.Kind == "update" || op.Kind == "syncupdate":
			// A syncupdate operation carries its staged update deliveries
			// directly: a delivery under it is this stage's instruction to
			// upgrade this member, executed exactly like a plain update.
			if Version == op.Target {
				break
			}
			raw, e := os.ReadFile(s.updateRequestPath())
			if e == nil && strings.TrimSpace(string(raw)) == op.Target {
				break
			}
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				err = e
				break
			}
			if e == nil {
				err = errUpdatePending
				break
			}
			err = s.writeUpdateRequest(op.Target)
		case op.Kind == "restore":
			// After a process crash the new process already reloaded the restored
			// CA/settings; accepting the old restart request must not restart it again.
		case op.Kind == "suppress":
			// Sync upgrade: silence this machine's failover watchdog for
			// the primary's planned restart. Accepted means the file is
			// on disk and the suppression is in effect.
			err = s.writeFailoverSuppress(op.ID)
		case op.Kind == "unsuppress":
			err = s.clearFailoverSuppress()
		default:
			err = errors.New("未知集群操作")
		}
		if err != nil {
			s.st.DB.Model(&d).Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "last_error": truncate(err.Error(), 255), "next_at": now.Add(15 * time.Second)})
			continue
		}
		r := s.st.DB.Model(&d).Where("status = ?", "pending").Updates(map[string]any{"status": "accepted", "accepted_at": now, "last_error": "", "attempts": gorm.Expr("attempts + 1")})
		if r.Error != nil || r.RowsAffected != 1 {
			continue
		}
		if op.Kind == "restore" && s.startedAt.Before(op.CreatedAt) {
			go func() { time.Sleep(time.Second); s.restartNow() }()
		}
	}
}

func (s *Server) hasClusterDelivery(kind, target string) bool {
	var n int64
	return s.st.DB.Model(&store.ClusterDelivery{}).Where("member_id = ? AND operation_id IN (?)", s.memberID, s.st.DB.Model(&store.ClusterOperation{}).Select("id").Where("kind = ? AND target = ? AND expires_at > ?", kind, target, time.Now().UTC())).Count(&n).Error == nil && n > 0
}

// inferUpdateOutcome concludes an accepted update delivery from the
// member table when the authoritative local receipt never arrived — the
// instance was killed mid-upgrade, or runs a build without the receipt
// write-back. since is when the current upgrade phase began. It reports
// ("", false) while the outcome is still open: the member has not
// restarted yet, is mid-restart, or has vanished (the phase timeout
// settles those). A member back at the target version is done; a member
// that restarted during the phase and came back online on another
// version was rolled back by its updater.
func inferUpdateOutcome(db *gorm.DB, d store.ClusterDelivery, target string, since time.Time) (string, bool) {
	if d.Status != "accepted" {
		return "", false
	}
	var m store.ClusterMember
	if err := db.Where("member_id = ?", d.MemberID).Order("seen_at desc").First(&m).Error; err != nil {
		return "", false
	}
	if m.Version == target {
		return "done", true
	}
	if time.Since(m.SeenAt) < memberOnline && m.StartedAt.After(since) {
		return "failed", true
	}
	return "", false
}

// saveClusterOperation runs in the same transaction as a restore when needed.
func saveClusterOperation(tx *gorm.DB, op store.ClusterOperation, memberID, procID string, includeSelf bool) (bool, error) {
	return saveClusterOperationScoped(tx, op, memberID, procID, includeSelf, nil)
}

// saveClusterOperationScoped is saveClusterOperation with the deliveries
// restricted to the given member ids (only == nil keeps every online
// member). Scoped operations never target legacy instances: a staged
// delivery is meaningless to an instance that cannot acknowledge it.
func saveClusterOperationScoped(tx *gorm.DB, op store.ClusterOperation, memberID, procID string, includeSelf bool, only map[string]bool) (bool, error) {
	legacy := false
	var members []store.ClusterMember
	if e := tx.Where("seen_at > ?", op.CreatedAt.Add(-memberOnline)).Find(&members).Error; e != nil {
		return false, e
	}
	targets := map[string]string{}
	for _, m := range members {
		if only != nil {
			if m.MemberID != "" && only[m.MemberID] {
				targets[m.MemberID] = m.Version
			}
			continue
		}
		if m.MemberID == "" && m.InstanceID != procID {
			legacy = true
			targets[m.InstanceID] = m.Version
			continue
		}
		if m.MemberID != "" && (includeSelf || m.MemberID != memberID) {
			targets[m.MemberID] = m.Version
		}
	}
	if only != nil {
		if only[memberID] {
			targets[memberID] = Version
		}
	} else if includeSelf {
		targets[memberID] = Version
	}
	if e := tx.Create(&op).Error; e != nil {
		return false, e
	}
	for id, version := range targets {
		d := store.ClusterDelivery{OperationID: op.ID, MemberID: id, BeforeVersion: version, Status: "pending", NextAt: op.CreatedAt}
		if e := tx.Create(&d).Error; e != nil {
			return false, e
		}
	}
	return legacy, nil
}

// queueClusterOperationScoped queues an operation whose deliveries go
// only to the given members. Unlike queueClusterOperation it never falls
// back to the legacy bus broadcasts: staged operations (suppress,
// unsuppress) must not be mistaken for a restore or a parallel update by
// instances that predate member ids.
func (s *Server) queueClusterOperationScoped(kind, target string, memberIDs []string) (string, error) {
	now := time.Now().UTC()
	op := store.ClusterOperation{ID: store.NewID(), Kind: kind, Target: target, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	only := make(map[string]bool, len(memberIDs))
	for _, id := range memberIDs {
		only[id] = true
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		_, e := saveClusterOperationScoped(tx, op, s.memberID, s.procID, false, only)
		return e
	})
	if err != nil {
		return "", err
	}
	if s.bus != nil {
		_ = s.bus.publish("cluster-ops", op.ID)
	}
	return op.ID, nil
}

// addSyncDeliveries stages update deliveries of a sync update operation
// for one phase's members. It is idempotent: a phase retried after a
// partial failure or a leader change re-adds nothing.
func (s *Server) addSyncDeliveries(op store.ClusterOperation, memberIDs []string) error {
	if len(memberIDs) == 0 {
		return nil
	}
	members := s.onlineMemberRows()
	now := time.Now().UTC()
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		for _, id := range memberIDs {
			d := store.ClusterDelivery{OperationID: op.ID, MemberID: id, BeforeVersion: members[id].Version, Status: "pending", NextAt: now}
			if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&d).Error; e != nil {
				return e
			}
		}
		return nil
	})
}
