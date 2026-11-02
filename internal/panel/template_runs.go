package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

const templatePlanTTL = 10 * time.Minute

type frozenTemplateNode struct {
	Plan   nodePlan
	Inputs string
	Models []*model.Inbound
	Items  []store.TemplateItem
}

type frozenTemplate struct {
	Template   store.InboundTemplate
	References string
	Options    planOptions
	Groups     []string
	Nodes      []frozenTemplateNode
}

type templateNodeResult struct {
	NodeID   string `json:"nodeId"`
	Written  bool   `json:"written"`
	Revision int64  `json:"revision"`
	Error    string `json:"error,omitempty"`
}

func templateHash(value any) string {
	b, _ := json.Marshal(value)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func templatePlanSignature(tpl store.InboundTemplate) string {
	// Label bindings affect future enrollments, not this selected-node plan.
	return templateHash([]any{tpl.ID, tpl.Name, tpl.Description, tpl.Items, tpl.Groups})
}

func (s *Server) templateReferences(tpl *store.InboundTemplate, opt *planOptions, groups []string) (string, error) {
	values := []any{}
	for _, id := range groups {
		var g store.Group
		if err := s.st.DB.Select("id").First(&g, "id = ?", id).Error; err != nil {
			return "", fmt.Errorf("授权分组已变化，请重新选择")
		}
		values = append(values, g.ID)
	}
	for _, it := range tpl.Items.V {
		if strings.HasPrefix(it.Cert, "wildcard:") {
			var cert store.Certificate
			if err := s.st.DB.First(&cert, "id = ?", strings.TrimPrefix(it.Cert, "wildcard:")).Error; err != nil {
				return "", err
			}
			values = append(values, []any{cert.ID, cert.UpdatedAt})
		}
		for _, ep := range it.Endpoints {
			switch ep.Kind {
			case "cdn":
				id := orDefault(ep.CDNProfileID, opt.CDNProfileID)
				if id == "" {
					continue
				}
				var profile store.CDNProfile
				if err := s.st.DB.First(&profile, "id = ?", id).Error; err != nil {
					return "", err
				}
				if cdnTunnel(&profile) {
					return "", fmt.Errorf("Tunnel 配置档当前不支持模板部署，请在入站页面创建 WS 入口")
				}
				values = append(values, profile)
			case "relay":
				var chain store.Chain
				if err := s.st.DB.First(&chain, "id = ?", ep.RelayChainID).Error; err != nil {
					return "", err
				}
				values = append(values, []any{chain.ID, chain.Mode, chain.Hops})
			}
		}
	}
	return templateHash(values), nil
}

func (s *Server) templateRevisionError(nodeID string, rev int64) string {
	var revision store.ConfigRevision
	if s.st.DB.Where("node_id = ? AND rev = ? AND status = ?", nodeID, rev, "failed").First(&revision).Error == nil {
		return orDefault(revision.Error, "节点应用配置失败")
	}
	return ""
}

// Only configuration goes into the fingerprint. Heartbeats and traffic must
// not invalidate a preview while an administrator is reading it.
func templateNodeInputs(db *gorm.DB, nodeID string) (string, error) {
	var node struct {
		ID, Name, Address, Domain, Region string
		DefaultCore                       model.CoreType
	}
	if err := db.Model(&store.Node{}).Where("id = ?", nodeID).Take(&node).Error; err != nil {
		return "", err
	}
	var cores []struct {
		Type    model.CoreType
		Version string
	}
	if err := db.Model(&store.Core{}).Where("node_id = ?", nodeID).Order("type").Find(&cores).Error; err != nil {
		return "", err
	}
	var ins []store.Inbound
	if err := db.Where("node_id = ?", nodeID).Order("id").Find(&ins).Error; err != nil {
		return "", err
	}
	var forwards []struct {
		ID                                      string
		ListenPort                              int
		Network, Engine, TunnelRole, TunnelType string
	}
	if err := db.Model(&store.ForwardRule{}).Where("node_id = ?", nodeID).Order("id").Find(&forwards).Error; err != nil {
		return "", err
	}
	var l4 []struct {
		ID   string
		Hops string
	}
	if err := db.Model(&store.Chain{}).Select("id, hops").Where("mode = ?", "l4").Order("id").Find(&l4).Error; err != nil {
		return "", err
	}
	return templateHash([]any{node, cores, ins, forwards, l4}), nil
}

func (s *Server) freezeTemplatePlan(tpl *store.InboundTemplate, opt *planOptions) (*store.TemplateRun, []nodePlan, error) {
	nodes, err := s.selectNodes(opt)
	if err != nil {
		return nil, nil, err
	}
	if len(nodes) == 0 || len(nodes) > 100 || len(nodes)*len(tpl.Items.V) > 2000 {
		return nil, nil, fmt.Errorf("每批请选择 1–100 个节点，最多生成 2000 个入站条目")
	}
	for _, node := range nodes {
		for key, target := range opt.RealityOverrides[node.ID] {
			found := false
			for _, item := range tpl.Items.V {
				if item.Key == key && item.Security.Type == model.SecReality {
					found = true
				}
			}
			if !found {
				return nil, nil, fmt.Errorf("节点 REALITY 覆盖引用了无效模板条目")
			}
			if err := validRealityOverride(target); err != nil {
				return nil, nil, err
			}
		}
	}
	snapshot := frozenTemplate{Template: *tpl, Options: *opt, Groups: groupsOr(tpl.Groups.V, s.defaultGroupIDs())}
	snapshot.References, err = s.templateReferences(tpl, opt, snapshot.Groups)
	if err != nil {
		return nil, nil, err
	}
	snapshot.Options.PlanID = ""
	var plans []nodePlan
	for i := range nodes {
		before, err := templateNodeInputs(s.st.DB, nodes[i].ID)
		if err != nil {
			return nil, nil, err
		}
		plan := s.planNode(tpl, &nodes[i], opt, false)
		after, err := templateNodeInputs(s.st.DB, nodes[i].ID)
		if err != nil {
			return nil, nil, err
		}
		if before != after {
			return nil, nil, fmt.Errorf("节点配置在预览过程中发生变化，请重新预览")
		}
		fn := frozenTemplateNode{Plan: plan, Inputs: before}
		for _, row := range plan.Items {
			fn.Models = append(fn.Models, row.inbound)
			fn.Items = append(fn.Items, row.item)
		}
		snapshot.Nodes = append(snapshot.Nodes, fn)
		plans = append(plans, plan)
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return nil, nil, err
	}
	run := store.TemplateRun{ID: store.NewID(), TemplateID: tpl.ID, State: "preview", Snapshot: s.st.Box.Seal(string(b)), Results: json.RawMessage("[]"), ExpiresAt: time.Now().Add(templatePlanTTL)}
	if err := s.st.DB.Create(&run).Error; err != nil {
		return nil, nil, err
	}
	// Preview keys expire quickly; deployment history is retained for 30 days.
	s.st.DB.Where("(state = ? AND expires_at < ?) OR created_at < ?", "preview", time.Now(), time.Now().Add(-30*24*time.Hour)).Delete(&store.TemplateRun{})
	return &run, plans, nil
}

func (s *Server) loadTemplateRun(id, templateID string) (*store.TemplateRun, *frozenTemplate, error) {
	var run store.TemplateRun
	if err := s.st.DB.Where("id = ? AND template_id = ?", id, templateID).First(&run).Error; err != nil {
		return nil, nil, err
	}
	raw, err := s.st.Box.Open(run.Snapshot)
	if err != nil {
		return nil, nil, err
	}
	var snapshot frozenTemplate
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return nil, nil, err
	}
	return &run, &snapshot, nil
}

func (s *Server) applyFrozenTemplate(c *gin.Context, tpl *store.InboundTemplate, opt *planOptions) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	run, snapshot, err := s.loadTemplateRun(opt.PlanID, tpl.ID)
	if err != nil {
		badRequest(c, fmt.Errorf("预览计划不存在，请重新预览"))
		return
	}
	if run.State == "finished" {
		s.templateRunJSON(c, run, snapshot)
		return
	}
	if run.State != "preview" {
		c.JSON(http.StatusConflict, gin.H{"error": "此计划正在执行，请查看应用状态"})
		return
	}
	cleanOpt := *opt
	cleanOpt.PlanID = ""
	refs, refErr := s.templateReferences(tpl, opt, snapshot.Groups)
	if time.Now().After(run.ExpiresAt) || refErr != nil || refs != snapshot.References || templatePlanSignature(snapshot.Template) != templatePlanSignature(*tpl) || templateHash(snapshot.Options) != templateHash(cleanOpt) {
		c.JSON(http.StatusConflict, gin.H{"error": "预览已过期，或模板/选项已变化，请重新预览"})
		return
	}
	for _, n := range snapshot.Nodes {
		current, err := templateNodeInputs(s.st.DB, n.Plan.NodeID)
		if err != nil || current != n.Inputs {
			c.JSON(http.StatusConflict, gin.H{"error": "节点配置已变化，请重新预览"})
			return
		}
		if opt.VerifyReality && frozenNodeHasReality(&n) {
			var check templateRealityCheck
			if !s.eph.get(templateRealityKey(run.ID, n.Plan.NodeID), &check) || !check.OK {
				c.JSON(http.StatusConflict, gin.H{"error": "请先从每个目标节点验证 REALITY 目标，或调整检测要求后重新预览"})
				return
			}
		}
	}
	claim := s.st.DB.Model(&store.TemplateRun{}).Where("id = ? AND state = ? AND expires_at > ?", run.ID, "preview", time.Now()).Update("state", "applying")
	if claim.Error != nil {
		serverError(c, claim.Error)
		return
	}
	if claim.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"error": "此计划已执行或正在执行"})
		return
	}
	results := make([]templateNodeResult, len(snapshot.Nodes))
	for i, n := range snapshot.Nodes {
		results[i] = templateNodeResult{NodeID: n.Plan.NodeID}
	}
	// Preserve partial progress even if a later write/publication fails.
	defer func() {
		b, _ := json.Marshal(results)
		s.st.DB.Model(&store.TemplateRun{}).Where("id = ? AND state = ?", run.ID, "applying").Updates(map[string]any{"state": "finished", "results": string(b)})
	}()
	for i := range snapshot.Nodes {
		results[i] = s.applyFrozenNode(snapshot, &snapshot.Nodes[i])
		b, _ := json.Marshal(results)
		if err := s.st.DB.Model(run).Update("results", json.RawMessage(b)).Error; err != nil {
			serverError(c, err)
			return
		}
	}
	b, _ := json.Marshal(results)
	if err := s.st.DB.Model(run).Updates(map[string]any{"state": "finished", "results": json.RawMessage(b)}).Error; err != nil {
		serverError(c, err)
		return
	}
	run.State, run.Results = "finished", b
	if opt.BindLabel != "" && !contains(tpl.BoundLabels.V, opt.BindLabel) {
		tpl.BoundLabels.V = append(tpl.BoundLabels.V, opt.BindLabel)
		if err := s.st.DB.Model(tpl).Update("bound_labels", tpl.BoundLabels).Error; err != nil {
			s.log.Warn("bind template label", "template", tpl.Name, "err", err)
		}
	}
	s.auditCtx(c, "template.apply", tpl.Name, "plan "+run.ID)
	s.templateRunJSON(c, run, snapshot)
}

func (s *Server) applyFrozenNode(snapshot *frozenTemplate, n *frozenTemplateNode) templateNodeResult {
	result := templateNodeResult{NodeID: n.Plan.NodeID}
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", n.Plan.NodeID).Error; err != nil {
		result.Error = err.Error()
		return result
	}
	ids := []string{}
	for j, in := range n.Models {
		if in != nil && n.Plan.Items[j].Result != "skip" {
			ids = append(ids, in.ID)
		}
	}
	if len(ids) > 0 {
		var count int64
		if err := s.st.DB.Model(&store.Inbound{}).Where("id IN ? AND template_id = ?", ids, snapshot.Template.ID).Count(&count).Error; err != nil {
			result.Error = err.Error()
			return result
		}
		if count == int64(len(ids)) {
			result.Written = true
			var err error
			result.Revision, _, err = s.Publish(node.ID, "recover template "+snapshot.Template.Name, false)
			if err != nil {
				result.Error = err.Error()
			}
			return result
		}
		if count != 0 {
			result.Error = "原计划的入站已部分变更，请重新预览"
			return result
		}
	}
	created := 0
	for j, m := range n.Models {
		if m == nil || n.Plan.Items[j].Result == "skip" {
			continue
		}
		if m.Security.Type == model.SecTLS {
			cert, _, err := s.certForPolicy(n.Items[j].Cert, m.Security.ServerName)
			if err != nil {
				result.Error = err.Error()
				return result
			}
			expected := n.Plan.Items[j].Cert
			if (strings.HasPrefix(expected, "ACME") && cert.Provider != "acme") || (strings.HasPrefix(expected, "自签") && cert.Provider != "selfsigned") {
				result.Error = "证书策略配置已变化，请重新预览"
				return result
			}
			m.Security.CertificateID = cert.ID
		}
		if err := s.validateInbound(&node, m, ""); err != nil {
			result.Error = err.Error()
			return result
		}
		created++
	}
	if created == 0 {
		return result
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "sqlite" {
			// A deferred SQLite transaction cannot upgrade its read snapshot
			// after another replica commits (SQLITE_BUSY_SNAPSHOT). Reserve the
			// writer before fingerprinting, so unrelated heartbeats cannot turn
			// an otherwise valid plan into a partially applied batch.
			r := tx.Exec("UPDATE nodes SET net_last_rx = net_last_rx WHERE id = ?", node.ID)
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return gorm.ErrRecordNotFound
			}
		} else if tx.Dialector.Name() == "postgres" {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&store.Node{}, "id = ?", node.ID).Error; err != nil {
				return err
			}
		}
		inputs, err := templateNodeInputs(tx, node.ID)
		if err != nil {
			return err
		}
		if inputs != n.Inputs {
			return fmt.Errorf("节点配置已变化，请重新预览")
		}
		for j, m := range n.Models {
			if m == nil || n.Plan.Items[j].Result == "skip" {
				continue
			}
			row := store.Inbound{ID: m.ID, TemplateID: snapshot.Template.ID, TemplateItemKey: n.Plan.Items[j].ItemKey, OverriddenFields: store.NewJSON([]string{})}
			if _, ok := snapshot.Options.RealityOverrides[node.ID][n.Plan.Items[j].ItemKey]; ok {
				row.OverriddenFields = store.NewJSON([]string{"security.realityDest", "security.realityServerNames"})
			}
			fromModel(m, &row)
			s.sealInbound(&row)
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			eps := n.Items[j].Endpoints
			if len(eps) == 0 {
				eps = []store.EndpointTemplate{{Kind: "direct"}}
			}
			for _, e := range eps {
				ep := store.Endpoint{ID: store.NewID(), InboundID: row.ID, Kind: e.Kind, Address: expandVars(e.Address, &node, "", nil), Port: e.Port, CDNProfileID: orDefault(e.CDNProfileID, snapshot.Options.CDNProfileID), RelayChainID: e.RelayChainID}
				if e.Kind == "cdn" && !cdnEndpointUsable(tx, m, ep.CDNProfileID, ep.Port) {
					continue
				}
				if e.Kind == "relay" && ep.RelayChainID == "" {
					continue
				}
				assignCDNHost(tx, &ep, node.ID)
				if err := tx.Create(&ep).Error; err != nil {
					return err
				}
			}
			for _, g := range snapshot.Groups {
				if err := tx.Create(&store.GroupInbound{GroupID: g, InboundID: row.ID}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Written = true
	result.Revision, _, err = s.Publish(node.ID, "template plan "+snapshot.Template.Name, false)
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func (s *Server) templateRunJSON(c *gin.Context, run *store.TemplateRun, snapshot *frozenTemplate) {
	var results []templateNodeResult
	_ = json.Unmarshal(run.Results, &results)
	byNode := map[string]templateNodeResult{}
	for _, r := range results {
		byNode[r.NodeID] = r
	}
	plans := make([]nodePlan, 0, len(snapshot.Nodes))
	for _, frozen := range snapshot.Nodes {
		p := frozen.Plan
		p.Items = append([]PlannedInbound(nil), p.Items...)
		r := byNode[p.NodeID]
		p.Error, p.Revision = r.Error, r.Revision
		switch {
		case r.Error != "":
			p.ApplyStatus = "error"
		case !r.Written:
			p.ApplyStatus = "skipped"
			if run.State == "preview" {
				p.ApplyStatus = ""
			}
			if run.State == "applying" {
				p.ApplyStatus = "pending"
			}
		default:
			var node store.Node
			if err := s.st.DB.First(&node, "id = ?", p.NodeID).Error; err != nil {
				p.ApplyStatus, p.Error = "error", "节点已删除"
				break
			}
			p.ApplyStatus = "pending"
			if r.Revision > 0 && node.AppliedRev >= r.Revision {
				p.ApplyStatus = "applied"
			} else if failed := s.templateRevisionError(node.ID, r.Revision); failed != "" {
				p.ApplyStatus, p.Error = "error", failed
			} else if !s.hub.Online(node.ID) {
				p.ApplyStatus = "offline"
			}
		}
		plans = append(plans, p)
	}
	checks := map[string]templateRealityCheck{}
	for _, node := range snapshot.Nodes {
		var check templateRealityCheck
		if s.eph.get(templateRealityKey(run.ID, node.Plan.NodeID), &check) {
			checks[node.Plan.NodeID] = check
		}
	}
	c.JSON(http.StatusOK, gin.H{"planId": run.ID, "state": run.State, "expiresAt": run.ExpiresAt, "plans": plans, "summary": summarize(plans), "checks": checks})
}

func (s *Server) getTemplateRun(c *gin.Context) {
	run, snapshot, err := s.loadTemplateRun(c.Param("run"), c.Param("id"))
	if err != nil {
		dbError(c, err)
		return
	}
	s.templateRunJSON(c, run, snapshot)
}

func (s *Server) listTemplateRuns(c *gin.Context) {
	var runs []store.TemplateRun
	if err := s.st.DB.Select("id, template_id, state, expires_at, created_at, updated_at").Where("template_id = ? AND state <> ?", c.Param("id"), "preview").Order("created_at desc").Limit(20).Find(&runs).Error; err != nil {
		serverError(c, err)
		return
	}
	if runs == nil {
		runs = []store.TemplateRun{}
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}

// A retry reuses successful database writes and publishes only failed nodes.
// It never recreates the inbounds of a node that was already committed.
func (s *Server) retryTemplateRun(c *gin.Context) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	run, snapshot, err := s.loadTemplateRun(c.Param("run"), c.Param("id"))
	if err != nil {
		dbError(c, err)
		return
	}
	if run.State != "finished" {
		c.JSON(http.StatusConflict, gin.H{"error": "此计划尚未执行完毕，请稍后刷新"})
		return
	}
	claim := s.st.DB.Model(&store.TemplateRun{}).Where("id = ? AND state = ?", run.ID, "finished").Update("state", "applying")
	if claim.Error != nil {
		serverError(c, claim.Error)
		return
	}
	if claim.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"error": "此计划正在重试"})
		return
	}
	var results []templateNodeResult
	if err := json.Unmarshal(run.Results, &results); err != nil {
		serverError(c, err)
		return
	}
	for i := range results {
		r := &results[i]
		if r.Written {
			var node store.Node
			if s.st.DB.First(&node, "id = ?", r.NodeID).Error != nil {
				r.Error = "节点已删除"
				continue
			}
			if r.Revision > 0 && node.AppliedRev >= r.Revision {
				r.Error = ""
				continue
			}
			if r.Error == "" && s.templateRevisionError(node.ID, r.Revision) == "" {
				continue
			}
			r.Revision, _, err = s.Publish(node.ID, "retry template "+snapshot.Template.Name, true)
			r.Error = ""
			if err != nil {
				r.Error = err.Error()
			}
			continue
		}
		if r.Error == "" {
			continue
		} // every planned item was skipped
		var tpl store.InboundTemplate
		ref, refErr := s.templateReferences(&snapshot.Template, &snapshot.Options, snapshot.Groups)
		if time.Now().After(run.ExpiresAt) || s.st.DB.First(&tpl, "id = ?", run.TemplateID).Error != nil || templatePlanSignature(tpl) != templatePlanSignature(snapshot.Template) || refErr != nil || ref != snapshot.References {
			r.Error = "原计划已过期或模板已变化，请为失败节点重新预览"
			continue
		}
		for j := range snapshot.Nodes {
			if snapshot.Nodes[j].Plan.NodeID == r.NodeID {
				*r = s.applyFrozenNode(snapshot, &snapshot.Nodes[j])
				break
			}
		}
	}
	b, _ := json.Marshal(results)
	if err := s.st.DB.Model(run).Updates(map[string]any{"state": "finished", "results": json.RawMessage(b)}).Error; err != nil {
		serverError(c, err)
		return
	}
	run.State, run.Results = "finished", b
	s.auditCtx(c, "template.retry", run.TemplateID, run.ID)
	s.templateRunJSON(c, run, snapshot)
}
