package panel

import (
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type overviewIssue struct {
	Kind       string     `json:"kind"`
	ResourceID string     `json:"resourceId"`
	NodeID     string     `json:"nodeId,omitempty"`
	Label      string     `json:"label,omitempty"`
	State      string     `json:"state"`
	Code       string     `json:"code"`
	CheckedAt  *time.Time `json:"checkedAt,omitempty"`
}

type operationalOverview struct {
	Available bool      `json:"available"`
	CheckedAt time.Time `json:"checkedAt"`
	Relays    struct {
		Total           int `json:"total"`
		Passed          int `json:"passed"`
		Failed          int `json:"failed"`
		Untested        int `json:"untested"`
		Stale           int `json:"stale"`
		Running         int `json:"running"`
		Interrupted     int `json:"interrupted"`
		Unavailable     int `json:"unavailable"`
		Inactive        int `json:"inactive"`
		FreshForSeconds int `json:"freshForSeconds"`
	} `json:"relayBusiness"`
	Sampling struct {
		Total             int `json:"total"`
		Healthy           int `json:"healthy"`
		Stale             int `json:"stale"`
		Never             int `json:"never"`
		Inactive          int `json:"inactive"`
		Pending           int `json:"pending"`
		Legacy            int `json:"legacy"`
		StaleAfterSeconds int `json:"staleAfterSeconds"`
	} `json:"sampling"`
	Delivery struct {
		PaymentPending int64 `json:"paymentPending"`
		PaymentReview  int64 `json:"paymentReview"`
		RefundPending  int64 `json:"refundPending"`
		ClusterPending int64 `json:"clusterPending"`
	} `json:"delivery"`
	Issues     []overviewIssue `json:"issues"`
	IssueCount int             `json:"issueCount"`
	Truncated  bool            `json:"truncated"`
}

func (s *Server) loadOperationalOverview(now time.Time) (operationalOverview, error) {
	out := operationalOverview{CheckedAt: now.UTC(), Issues: []overviewIssue{}}
	out.Relays.FreshForSeconds = int(relayBusinessFreshFor.Seconds())
	out.Sampling.StaleAfterSeconds = int(nodeNetStaleAfter.Seconds())
	opts := &sql.TxOptions{ReadOnly: true}
	if s.st.DB.Dialector.Name() == "postgres" {
		opts.Isolation = sql.LevelRepeatableRead
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var nodes []store.Node
		if err := tx.Find(&nodes).Error; err != nil {
			return err
		}
		byNode := map[string]store.Node{}
		for _, n := range nodes {
			byNode[n.ID] = n
			if n.Revoked {
				continue
			}
			out.Sampling.Total++
			if n.Status == "pending" {
				out.Sampling.Pending++
				continue
			}
			if n.Status == "offline" {
				out.Sampling.Inactive++
				continue
			}
			health := nodeNetHealthOf(&n, now)
			if health.Mode == "legacy" || health.Mode == "epoch" {
				out.Sampling.Legacy++
			}
			switch health.State {
			case "healthy":
				out.Sampling.Healthy++
			case "stale":
				out.Sampling.Stale++
			default:
				out.Sampling.Never++
			}
			if health.State != "healthy" {
				out.Issues = append(out.Issues, overviewIssue{Kind: "sampling", ResourceID: n.ID, NodeID: n.ID, Label: n.Name, State: health.State, Code: "sampling_" + health.State, CheckedAt: health.LastSampleAt})
			}
		}
		var chains []store.Chain
		if err := tx.Where("managed = ?", true).Order("id").Find(&chains).Error; err != nil {
			return err
		}
		var ins []store.Inbound
		if err := tx.Find(&ins).Error; err != nil {
			return err
		}
		byIn := map[string]store.Inbound{}
		for _, in := range ins {
			byIn[in.ID] = in
		}
		var endpoints []store.Endpoint
		if err := tx.Where("kind = ?", "relay").Order("created_at,id").Find(&endpoints).Error; err != nil {
			return err
		}
		type endpointKey struct{ chainID, inboundID string }
		byEndpoint := map[endpointKey]store.Endpoint{}
		for _, ep := range endpoints {
			key := endpointKey{ep.RelayChainID, ep.InboundID}
			if _, ok := byEndpoint[key]; !ok {
				byEndpoint[key] = ep
			}
		}
		var records []store.RelayBusinessStatus
		if err := tx.Find(&records).Error; err != nil {
			return err
		}
		byChain := map[string]store.RelayBusinessStatus{}
		for _, record := range records {
			byChain[record.ChainID] = record
		}
		for _, ch := range chains {
			out.Relays.Total++
			inboundID := ""
			if len(ch.Hops.V) > 0 {
				inboundID = ch.Hops.V[len(ch.Hops.V)-1].InboundID
			}
			state, code, checked, nodeID := relayOverviewState(ch, byNode, byIn, byEndpoint[endpointKey{ch.ID, inboundID}], byChain[ch.ID], now)
			switch state {
			case "passed":
				out.Relays.Passed++
			case "failed":
				out.Relays.Failed++
			case "stale":
				out.Relays.Stale++
			case "running":
				out.Relays.Running++
			case "interrupted":
				out.Relays.Interrupted++
			case "unavailable":
				out.Relays.Unavailable++
			case "inactive":
				out.Relays.Inactive++
			default:
				out.Relays.Untested++
			}
			if state != "passed" && state != "untested" && state != "inactive" {
				out.Issues = append(out.Issues, overviewIssue{Kind: "relay_business", ResourceID: ch.ID, NodeID: nodeID, Label: ch.Name, State: state, Code: code, CheckedAt: checked})
			}
		}
		for _, q := range []struct {
			model any
			where string
			args  []any
			count *int64
			code  string
		}{
			{&store.PaymentReceipt{}, "status = ? AND created_at < ?", []any{"pending", now.Add(-time.Minute)}, &out.Delivery.PaymentPending, "payment_pending"},
			{&store.PaymentReceipt{}, "status = ?", []any{"review"}, &out.Delivery.PaymentReview, "payment_review"},
			{&store.RefundJob{}, "status <> ? AND created_at < ?", []any{"done", now.Add(-time.Minute)}, &out.Delivery.RefundPending, "refund_pending"},
			{&store.ClusterDelivery{}, "status = ? AND operation_id IN (?)", []any{"pending", tx.Model(&store.ClusterOperation{}).Select("id").Where("created_at < ?", now.Add(-time.Minute))}, &out.Delivery.ClusterPending, "cluster_pending"},
		} {
			if err := tx.Model(q.model).Where(q.where, q.args...).Count(q.count).Error; err != nil {
				return err
			}
			if *q.count > 0 {
				out.Issues = append(out.Issues, overviewIssue{Kind: "delivery", ResourceID: q.code, State: "attention", Code: q.code})
			}
		}
		var operations []store.OperationalStatus
		if err := tx.Where("failed = ?", true).Find(&operations).Error; err != nil {
			return err
		}
		for _, operation := range operations {
			if strings.HasPrefix(operation.Key, "agent-traffic:") {
				n, ok := byNode[strings.TrimPrefix(operation.Key, "agent-traffic:")]
				if !ok || n.Revoked {
					continue
				}
			}
			out.Issues = append(out.Issues, overviewIssue{Kind: "operation", ResourceID: operation.Key, State: "failed", Code: "operation_failed", CheckedAt: &operation.UpdatedAt})
		}
		return nil
	}, opts)
	if err != nil {
		return operationalOverview{CheckedAt: now.UTC(), Issues: []overviewIssue{}}, err
	}
	out.Available = true
	sort.Slice(out.Issues, func(i, j int) bool {
		a, b := out.Issues[i], out.Issues[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.ResourceID < b.ResourceID
	})
	out.IssueCount = len(out.Issues)
	if len(out.Issues) > 200 {
		out.Issues = out.Issues[:200]
		out.Truncated = true
	}
	return out, nil
}

func relayOverviewState(ch store.Chain, nodes map[string]store.Node, ins map[string]store.Inbound, ep store.Endpoint, record store.RelayBusinessStatus, now time.Time) (state, code string, checked *time.Time, nodeID string) {
	checked = record.CheckedAt
	if ch.Disabled {
		return "inactive", "disabled", checked, ""
	}
	if len(ch.Hops.V) < 2 {
		return "unavailable", "invalid_path", checked, ""
	}
	in, found := ins[ch.Hops.V[len(ch.Hops.V)-1].InboundID]
	if !found {
		return "unavailable", "missing_inbound", checked, ""
	}
	nodeID = in.NodeID
	if ch.Hops.V[len(ch.Hops.V)-1].NodeID != in.NodeID {
		return "unavailable", "invalid_path", checked, nodeID
	}
	if !in.Enabled {
		return "inactive", "inbound_disabled", checked, nodeID
	}
	if ep.ID == "" || ep.InboundID != in.ID {
		return "unavailable", "missing_endpoint", checked, nodeID
	}
	for _, hop := range ch.Hops.V {
		n, ok := nodes[hop.NodeID]
		if !ok || n.Revoked || (n.Status != "online" && n.Status != "syncing") || n.LastError != "" || n.DesiredRev <= 0 || n.AppliedRev < n.DesiredRev || n.LastSeen == nil || now.Sub(*n.LastSeen) > 90*time.Second {
			return "unavailable", "deployment_not_ready", checked, nodeID
		}
	}
	landing := nodes[in.NodeID]
	if in.Core != model.CoreXray || in.Protocol.V.Type != model.ProtoVLESS || !contains(landing.AgentFeatures.V, model.RelayBusinessFeature) {
		return "unavailable", "unsupported", checked, nodeID
	}
	if record.ChainID == "" {
		return "untested", "never_tested", nil, nodeID
	}
	entry := nodes[ch.Hops.V[0].NodeID]
	address, port := nodeHost(&entry), ch.Hops.V[0].Port
	if ep.Address != "" {
		address = ep.Address
	}
	if ep.Port > 0 {
		port = ep.Port
	}
	req := model.RelayBusinessRequest{EntryAddress: address, EntryPort: port, ServerName: orDefault(in.Security.V.ServerName, landing.Domain)}
	if record.InboundID != in.ID || record.Fingerprint != relayBusinessFingerprint(ch, in, nodes, req) {
		return "stale", "configuration_changed", checked, nodeID
	}
	if record.State == "running" {
		if record.StartedAt.After(now.Add(relayBusinessRunningFor)) {
			return "interrupted", "clock_invalid", nil, nodeID
		}
		if now.Sub(record.StartedAt) >= relayBusinessRunningFor {
			return "interrupted", "result_missing", nil, nodeID
		}
		return "running", "in_progress", nil, nodeID
	}
	if checked != nil && checked.After(now.Add(relayBusinessRunningFor)) {
		return "stale", "clock_invalid", checked, nodeID
	}
	if checked == nil || now.Sub(*checked) >= relayBusinessFreshFor {
		return "stale", "result_expired", checked, nodeID
	}
	switch record.State {
	case "passed":
		if record.BusinessVerified && record.CleanupVerified && len(record.FailedChecks.V) == 0 {
			return "passed", "verified", checked, nodeID
		}
	case "interrupted":
		return "interrupted", "execution_interrupted", checked, nodeID
	case "unsupported":
		return "unavailable", "unsupported", checked, nodeID
	}
	if !record.CleanupVerified {
		return "failed", "cleanup_unconfirmed", checked, nodeID
	}
	return "failed", "business_failed", checked, nodeID
}

func (s *Server) statsOperational(c *gin.Context) {
	out, err := s.loadOperationalOverview(time.Now())
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(200, out)
}
