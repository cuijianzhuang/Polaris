package panel

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type relayChangeReference struct {
	Kind       string `json:"kind"`
	ResourceID string `json:"resourceId"`
	NodeID     string `json:"nodeId,omitempty"`
	TargetTag  string `json:"targetTag,omitempty"`
}

type relayChangeConflict struct{ references []relayChangeReference }

func (e relayChangeConflict) Error() string {
	return "链路资源仍被其他配置引用，请先解除引用"
}

type relayPreviewStale struct{}

func (relayPreviewStale) Error() string { return "预览后配置已变化，请重新预览" }

func relayChangeError(c *gin.Context, err error) {
	var conflict relayChangeConflict
	var stale relayPreviewStale
	if errors.As(err, &conflict) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "references": conflict.references})
		return
	}
	if errors.As(err, &stale) {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": err.Error()})
		return
	}
	if store.IsDuplicate(err) {
		dbError(c, err)
		return
	}
	relaySetupError(c, err)
}

type relayChangeNode struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Status            string `json:"status"`
	DesiredRev        int64  `json:"desiredRev"`
	AppliedRev        int64  `json:"appliedRev"`
	OtherPaths        int    `json:"otherPaths"`
	OtherEnabledPaths int    `json:"otherEnabledPaths"`
}

type relayChangePreview struct {
	Action                     string                 `json:"action"`
	ETag                       string                 `json:"etag"`
	CheckedAt                  time.Time              `json:"checkedAt"`
	ChainIDs                   []string               `json:"chainIds"`
	ChangedPaths               int                    `json:"changedPaths"`
	Nodes                      []relayChangeNode      `json:"nodes"`
	EndpointIDs                []string               `json:"endpointIds"`
	OutboundIDs                []string               `json:"outboundIds"`
	RoutingRuleIDs             []string               `json:"routingRuleIds"`
	ServiceUserIDs             []string               `json:"serviceUserIds"`
	Ports                      []map[string]any       `json:"ports"`
	RemainingRelayPaths        int                    `json:"remainingRelayPaths"`
	RemainingEnabledRelayPaths int                    `json:"remainingEnabledRelayPaths"`
	RelayOnly                  bool                   `json:"relayOnly"`
	SourceProtection           bool                   `json:"sourceProtection"`
	Blocked                    bool                   `json:"blocked"`
	References                 []relayChangeReference `json:"references"`
	Warnings                   []string               `json:"warnings"`
}

func validRelayChangeAction(action string) bool {
	return action == "enable" || action == "disable" || action == "delete"
}

// Only configuration enters the ETag. Heartbeat times, traffic counters and
// health measurements must not invalidate a review every few seconds.
func buildRelayChangePreview(tx *gorm.DB, scope, action string, chains []store.Chain) (relayChangePreview, error) {
	out := relayChangePreview{Action: action, CheckedAt: time.Now().UTC(), ChainIDs: []string{}, Nodes: []relayChangeNode{}, EndpointIDs: []string{}, OutboundIDs: []string{}, RoutingRuleIDs: []string{}, ServiceUserIDs: []string{}, Ports: []map[string]any{}, References: []relayChangeReference{}, Warnings: []string{}}
	if !validRelayChangeAction(action) {
		return out, badInput{fmt.Errorf("action 只能是 enable、disable 或 delete")}
	}
	selected := map[string]bool{}
	affected := map[string]bool{}
	serviceIDs := []string{}
	inboundID := ""
	for _, ch := range chains {
		selected[ch.ID] = true
		out.ChainIDs = append(out.ChainIDs, ch.ID)
		for _, h := range ch.Hops.V {
			affected[h.NodeID] = true
		}
		if ch.ServiceUserID != "" {
			serviceIDs = append(serviceIDs, ch.ServiceUserID)
		}
		if len(ch.Hops.V) > 0 {
			inboundID = ch.Hops.V[len(ch.Hops.V)-1].InboundID
		}
		if action == "delete" || ch.Disabled == (action == "enable") {
			out.ChangedPaths++
		}
	}
	sort.Strings(out.ChainIDs)
	var in store.Inbound
	if err := tx.First(&in, "id = ?", inboundID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return out, err
	}
	out.RelayOnly, out.SourceProtection = in.RelayOnly, in.RelayAccess.V.Enabled
	var allChains []store.Chain
	if err := tx.Order("id").Find(&allChains).Error; err != nil {
		return out, err
	}
	var allNodes []store.Node
	if err := tx.Where("id IN ?", relayChangeKeys(affected)).Order("id").Find(&allNodes).Error; err != nil {
		return out, err
	}
	nodes := map[string]store.Node{}
	canonical := []any{scope, action, out.ChainIDs, in}
	for _, n := range allNodes {
		nodes[n.ID] = n
		canonical = append(canonical, []any{n.ID, n.Name, n.Address, n.Domain, n.Status, n.DesiredRev, n.AppliedRev, n.LastError, n.Revoked, n.NoFirewall, n.AgentFeatures, n.PublicIPv4, n.PublicIPv6})
	}
	refs := map[string][2]int{}
	for _, ch := range allChains {
		if selected[ch.ID] {
			config := ch
			config.Probe = store.NewJSON([]store.HopProbe{})
			config.ProbedAt = nil
			config.UpdatedAt = time.Time{}
			canonical = append(canonical, []any{config, ch.TunnelAuth})
			continue
		}
		touches := false
		seen := map[string]bool{}
		for _, h := range ch.Hops.V {
			if affected[h.NodeID] && !seen[h.NodeID] {
				v := refs[h.NodeID]
				v[0]++
				if !ch.Disabled {
					v[1]++
				}
				refs[h.NodeID] = v
				seen[h.NodeID] = true
				touches = true
			}
		}
		if touches {
			canonical = append(canonical, []any{ch.ID, ch.Managed, ch.Disabled, ch.Hops, ch.ServiceUserID})
		}
		if ch.Managed && len(ch.Hops.V) > 0 && ch.Hops.V[len(ch.Hops.V)-1].InboundID == inboundID {
			out.RemainingRelayPaths++
			if !ch.Disabled {
				out.RemainingEnabledRelayPaths++
			}
		}
	}
	for _, ch := range chains {
		if action != "delete" {
			out.RemainingRelayPaths++
			if action == "enable" {
				out.RemainingEnabledRelayPaths++
			}
		}
		if ch.Managed && ch.Mode == "l4" && in.ID != "" {
			for nodeID, rules := range managedRelayForwards(&ch, nodes, in, "") {
				for _, rule := range rules {
					out.Ports = append(out.Ports, map[string]any{"chainId": ch.ID, "nodeId": nodeID, "port": rule.Listen, "network": rule.ListenNetwork(), "public": !isLoopbackHost(rule.ListenIP), "role": rule.TunnelRole, "engine": orDefault(rule.Engine, "realm")})
				}
			}
		}
	}
	for _, id := range relayChangeKeys(affected) {
		n, exists := nodes[id]
		status := n.Status
		if !exists {
			status = "missing"
		}
		out.Nodes = append(out.Nodes, relayChangeNode{ID: id, Name: n.Name, Status: status, DesiredRev: n.DesiredRev, AppliedRev: n.AppliedRev, OtherPaths: refs[id][0], OtherEnabledPaths: refs[id][1]})
		if status == "offline" || status == "missing" || status == "pending" {
			out.Warnings = append(out.Warnings, "节点 "+id+" 尚未就绪；配置提交不代表该节点已经完成应用")
		}
	}
	var endpoints []store.Endpoint
	if err := tx.Where("relay_chain_id IN ?", out.ChainIDs).Order("id").Find(&endpoints).Error; err != nil {
		return out, err
	}
	for _, ep := range endpoints {
		out.EndpointIDs = append(out.EndpointIDs, ep.ID)
	}
	canonical = append(canonical, endpoints)
	var outs []store.Outbound
	if err := tx.Order("id").Find(&outs).Error; err != nil {
		return out, err
	}
	var rules []store.RoutingRule
	if err := tx.Where("node_id IN ? OR chain_id IN ?", append(relayChangeKeys(affected), model.GlobalNodeID), out.ChainIDs).Order("id").Find(&rules).Error; err != nil {
		return out, err
	}
	var bindings []store.Inbound
	if err := tx.Where("node_id IN ?", relayChangeKeys(affected)).Order("id").Find(&bindings).Error; err != nil {
		return out, err
	}
	ownedTags := map[string]map[string]bool{}
	for _, o := range outs {
		if selected[o.ChainID] {
			out.OutboundIDs = append(out.OutboundIDs, o.ID)
			if ownedTags[o.NodeID] == nil {
				ownedTags[o.NodeID] = map[string]bool{}
			}
			ownedTags[o.NodeID][o.Tag] = true
		}
		if affected[o.NodeID] || contains(serviceIDs, o.ServiceUserID) {
			canonical = append(canonical, o)
		}
	}
	for _, r := range rules {
		if selected[r.ChainID] {
			out.RoutingRuleIDs = append(out.RoutingRuleIDs, r.ID)
		}
	}
	canonical = append(canonical, rules)
	for _, binding := range bindings {
		canonical = append(canonical, []any{binding.ID, binding.NodeID, binding.OutboundTag, binding.UpdatedAt})
	}
	var services []store.User
	if len(serviceIDs) > 0 {
		if err := tx.Where("id IN ? AND is_service = ?", serviceIDs, true).Order("id").Find(&services).Error; err != nil {
			return out, err
		}
	}
	for _, user := range services {
		out.ServiceUserIDs = append(out.ServiceUserIDs, user.ID)
		canonical = append(canonical, []any{user.ID, user.UUID, user.Password, user.SSSeed, user.Status, user.IsService})
	}
	if action == "delete" {
		for _, o := range outs {
			if selected[o.ChainID] {
				continue
			}
			if contains(out.ServiceUserIDs, o.ServiceUserID) {
				out.References = append(out.References, relayChangeReference{Kind: "service_user_outbound", ResourceID: o.ID, NodeID: o.NodeID})
			}
			if len(ownedTags[o.NodeID]) == 0 {
				continue
			}
			deps, err := (model.Outbound{Tag: o.Tag, Type: o.Type, Settings: o.Settings, Detour: o.Detour}).OutboundDependencies()
			if err != nil {
				return out, fmt.Errorf("节点 %s 的出站依赖无法解析，请先修复", o.NodeID)
			}
			for _, tag := range deps {
				if ownedTags[o.NodeID][tag] {
					out.References = append(out.References, relayChangeReference{Kind: "outbound", ResourceID: o.ID, NodeID: o.NodeID, TargetTag: tag})
				}
			}
		}
		for _, r := range rules {
			if selected[r.ChainID] {
				continue
			}
			for nodeID, tags := range ownedTags {
				if (r.NodeID == nodeID || r.NodeID == model.GlobalNodeID) && tags[r.OutboundTag] {
					out.References = append(out.References, relayChangeReference{Kind: "routing_rule", ResourceID: r.ID, NodeID: r.NodeID, TargetTag: r.OutboundTag})
					break
				}
			}
		}
		for _, binding := range bindings {
			if ownedTags[binding.NodeID][binding.OutboundTag] {
				out.References = append(out.References, relayChangeReference{Kind: "inbound", ResourceID: binding.ID, NodeID: binding.NodeID, TargetTag: binding.OutboundTag})
			}
		}
		for _, ch := range allChains {
			if !selected[ch.ID] && contains(out.ServiceUserIDs, ch.ServiceUserID) {
				out.References = append(out.References, relayChangeReference{Kind: "service_user_chain", ResourceID: ch.ID})
			}
		}
	}
	sort.Slice(out.References, func(i, j int) bool {
		a, b := out.References[i], out.References[j]
		return a.Kind+a.ResourceID+a.NodeID+a.TargetTag < b.Kind+b.ResourceID+b.NodeID+b.TargetTag
	})
	sort.Slice(out.Ports, func(i, j int) bool {
		a, b := out.Ports[i], out.Ports[j]
		if a["chainId"] != b["chainId"] {
			return a["chainId"].(string) < b["chainId"].(string)
		}
		if a["nodeId"] != b["nodeId"] {
			return a["nodeId"].(string) < b["nodeId"].(string)
		}
		return a["port"].(int) < b["port"].(int)
	})
	canonical = append(canonical, out.References)
	out.ETag = "\"" + templateHash(canonical) + "\""
	out.Blocked = len(out.References) > 0
	if out.RelayOnly && out.RemainingEnabledRelayPaths == 0 {
		out.Warnings = append(out.Warnings, "操作后没有启用的受管中转；仅中转发布与来源保护不会因此自动关闭，落地直连不会自动开放")
	}
	return out, nil
}

func checkRelayChange(tx *gorm.DB, c *gin.Context, scope, action string, chains []store.Chain) error {
	if action != "delete" && c.GetHeader("If-Match") == "" {
		return nil
	}
	preview, err := buildRelayChangePreview(tx, scope, action, chains)
	if err != nil {
		return err
	}
	if tag := c.GetHeader("If-Match"); tag != "" {
		if len(tag) != 66 || !strings.HasPrefix(tag, "\"") || !strings.HasSuffix(tag, "\"") || strings.Trim(tag[1:65], "0123456789abcdef") != "" {
			return badInput{fmt.Errorf("If-Match 需要预览返回的单个强 ETag")}
		}
		if tag != preview.ETag {
			return relayPreviewStale{}
		}
	}
	if preview.Blocked {
		return relayChangeConflict{preview.References}
	}
	return nil
}

// Three preview routes share one read-only snapshot and reveal only selected
// resource identities, port metadata and reference counts, never credentials.
func (s *Server) previewRelayChange(c *gin.Context) {
	var req struct {
		Action string `json:"action"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if !validRelayChangeAction(req.Action) {
		badRequest(c, fmt.Errorf("action 只能是 enable、disable 或 delete"))
		return
	}
	scope := "relay"
	if c.Param("bid") != "" {
		scope = "batch"
	} else if c.Param("rid") == "" {
		scope = "chain"
	}
	if scope == "chain" && req.Action != "delete" {
		badRequest(c, fmt.Errorf("普通链路仅支持 delete 预览"))
		return
	}
	opts := &sql.TxOptions{ReadOnly: true}
	if s.st.DB.Dialector.Name() == "postgres" {
		opts.Isolation = sql.LevelRepeatableRead
	}
	var out relayChangePreview
	err := s.st.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var chains []store.Chain
		q := tx.Order("id")
		switch scope {
		case "batch":
			q = q.Where("batch_id = ? AND managed = ?", c.Param("bid"), true)
		case "relay":
			q = q.Where("id = ? AND managed = ?", c.Param("rid"), true)
		default:
			q = q.Where("id = ?", c.Param("id"))
		}
		if err := q.Find(&chains).Error; err != nil {
			return err
		}
		if len(chains) == 0 {
			return gorm.ErrRecordNotFound
		}
		if scope != "chain" {
			for _, ch := range chains {
				if len(ch.Hops.V) < 2 || ch.Hops.V[len(ch.Hops.V)-1].InboundID != c.Param("id") {
					return gorm.ErrRecordNotFound
				}
			}
		}
		var err error
		out, err = buildRelayChangePreview(tx, scope, req.Action, chains)
		return err
	}, opts)
	if err != nil {
		relayChangeError(c, err)
		return
	}
	c.Header("ETag", out.ETag)
	c.JSON(http.StatusOK, out)
}

func relayChangeKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
