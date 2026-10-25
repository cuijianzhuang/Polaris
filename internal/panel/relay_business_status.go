package panel

import (
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	relayBusinessTCP        = "https://www.gstatic.com/generate_204"
	relayBusinessUDP        = "1.1.1.1:53"
	relayBusinessFreshFor   = 15 * time.Minute
	relayBusinessRunningFor = 30 * time.Second
)

func relayBusinessFingerprint(ch store.Chain, in store.Inbound, nodes map[string]store.Node, req model.RelayBusinessRequest) string {
	values := []any{relayInputHash(in, nodes, &ch), in.Core, in.UpdatedAt, ch.UpdatedAt, ch.Mode, ch.Disabled, ch.Hops, ch.TunnelType, ch.TunnelAuth, ch.TargetRevs,
		req.EntryAddress, req.EntryPort, req.ServerName}
	for _, hop := range ch.Hops.V {
		n := nodes[hop.NodeID]
		values = append(values, []any{n.ID, n.Domain, n.DesiredRev, n.AppliedRev, n.AgentVersion, n.AgentFeatures})
	}
	return templateHash(values)
}

func (s *Server) beginRelayBusinessStatus(ch store.Chain, in store.Inbound, req model.RelayBusinessRequest) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		// The chain row serializes new attempts and deletion. A completing old
		// attempt can only update its own run ID, never replace a newer result.
		locked := tx.Model(&store.Chain{}).Where("id = ? AND managed = ?", ch.ID, true).UpdateColumn("id", gorm.Expr("id"))
		if locked.Error != nil {
			return locked.Error
		}
		if locked.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		nodes, err := relayNodes(tx, &ch)
		if err != nil {
			return err
		}
		row := store.RelayBusinessStatus{ChainID: ch.ID, InboundID: in.ID, NodeID: in.NodeID, RunID: req.ID,
			Fingerprint: relayBusinessFingerprint(ch, in, nodes, req), State: "running", StartedAt: time.Now().UTC(), FailedChecks: store.NewJSON([]string{})}
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
	})
}

func (s *Server) finishRelayBusinessStatus(chainID, runID, state string, result model.RelayBusinessResult) error {
	// Keep only recognized check codes, never the node's arbitrary messages,
	// target strings, temporary credentials or unknown extension fields.
	allowed := map[string]bool{"proxy_authentication": true, "proxy_transfer": true, "core_recovery": true, "proxy_tcp_business": true, "proxy_udp_business": true, "credential_cleanup": true, "execution": true}
	failed := []string{}
	seen := map[string]bool{}
	for _, check := range result.Checks {
		if check.State == "passed" {
			continue
		}
		kind := check.Kind
		if !allowed[kind] {
			kind = "execution"
		}
		if !seen[kind] {
			failed = append(failed, kind)
			seen[kind] = true
		}
	}
	if !result.CleanupVerified && !seen["credential_cleanup"] {
		failed = append(failed, "credential_cleanup")
	}
	switch state {
	case "passed", "failed", "unsupported", "interrupted":
	default:
		state = "failed"
	}
	verified := state == "passed" && result.BusinessVerified && result.CleanupVerified && len(failed) == 0
	if state == "passed" && !verified {
		state = "failed"
	}
	now := time.Now().UTC()
	return s.st.DB.Model(&store.RelayBusinessStatus{}).Where("chain_id = ? AND run_id = ?", chainID, runID).
		Updates(map[string]any{"state": state, "checked_at": now, "business_verified": verified, "cleanup_verified": result.CleanupVerified, "failed_checks": store.NewJSON(failed)}).Error
}
