package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Subscription view groups (订阅视图组), batch 1: model + admin CRUD, and the
// user-subscription pipeline for plan-bound groups.
//
// Provisional licensing: the design (§11.1) leaves open whether view groups
// belong to the community build or the pro edition. Batch 1 follows the
// status quo of user-subscription operator execution — community build, no
// feature gate. If that call changes, the gate goes in these handlers and
// in applyPlanViewGroups; nothing else moves.

const (
	maxViewGroupsPerBinding = 10
	maxViewGroupSteps       = 64
	maxViewGroupRefs        = 20
	maxViewGroupRuleJSON    = 48 << 10
)

// --- validation ---

func (s *Server) validateViewGroup(vg *store.SubViewGroup) error {
	vg.Name = strings.TrimSpace(vg.Name)
	vg.Remark = strings.TrimSpace(vg.Remark)
	if vg.Name == "" || len([]rune(vg.Name)) > 64 {
		return errors.New("视图组名称必填且最多 64 字")
	}
	if len([]rune(vg.Remark)) > 255 {
		return errors.New("备注最多 255 字")
	}
	sel := vg.Selector.V
	sel.Type = "filter"
	vg.Selector = store.NewJSON(sel)
	if err := sub.ValidateChain([]sub.Operator{sel}); err != nil {
		return fmt.Errorf("选择谓词无效：%w", err)
	}
	if len(vg.Operators.V) > maxViewGroupSteps {
		return fmt.Errorf("本地算子最多 %d 步", maxViewGroupSteps)
	}
	if err := sub.ValidateChain(vg.Operators.V); err != nil {
		return fmt.Errorf("本地算子无效：%w", err)
	}
	for _, k := range vg.Kinds.V {
		switch k {
		case "direct", "cdn", "relay":
		default:
			return fmt.Errorf("未知的节点形态 %q", k)
		}
	}
	ruleJSON, err := json.Marshal(struct {
		Selector  sub.Operator             `json:"selector"`
		Operators []sub.Operator           `json:"operators"`
		Refs      []store.SubProcessingRef `json:"refs"`
	}{sel, vg.Operators.V, vg.ProcessingGroups.V})
	if err != nil {
		return err
	}
	if len(ruleJSON) > maxViewGroupRuleJSON {
		return fmt.Errorf("规则 JSON 超过 %d KiB 上限", maxViewGroupRuleJSON>>10)
	}
	refs := vg.ProcessingGroups.V
	if len(refs) > maxViewGroupRefs {
		return fmt.Errorf("引用的处理组最多 %d 个", maxViewGroupRefs)
	}
	if len(refs) > 0 {
		ids := make([]string, 0, len(refs))
		seen := map[string]bool{}
		for _, r := range refs {
			if seen[r.GroupID] {
				return fmt.Errorf("处理组 %s 被重复引用", r.GroupID)
			}
			seen[r.GroupID] = true
			ids = append(ids, r.GroupID)
		}
		var groups []store.SubProcessingGroup
		if err := s.st.DB.Select("id", "revision").Where("id IN ?", ids).Find(&groups).Error; err != nil {
			return err
		}
		have := map[string]int64{}
		for _, g := range groups {
			have[g.ID] = g.Revision
		}
		for _, r := range refs {
			rev, ok := have[r.GroupID]
			if !ok {
				return fmt.Errorf("引用的处理组不存在：%s", r.GroupID)
			}
			if r.Revision < 0 || r.Revision > rev {
				return fmt.Errorf("处理组 %s 没有版本 v%d", r.GroupID, r.Revision)
			}
		}
	}
	return nil
}

// viewGroupReferences lists what binds a view group: plan/group names and
// profile names. Used by delete protection.
func (s *Server) viewGroupReferences(id string) (refs []string, err error) {
	var groups []store.Group
	if err := s.st.DB.Select("name", "view_groups").Where("view_groups IS NOT NULL AND view_groups != '' AND view_groups != '[]' AND view_groups != 'null'").Find(&groups).Error; err != nil {
		return nil, err
	}
	for _, g := range groups {
		for _, bound := range g.ViewGroups.V {
			if bound == id {
				refs = append(refs, "套餐/分组「"+g.Name+"」")
				break
			}
		}
	}
	var profiles []store.SubProfile
	if err := s.st.DB.Select("name", "view_groups").Where("view_groups IS NOT NULL AND view_groups != '' AND view_groups != '[]' AND view_groups != 'null'").Find(&profiles).Error; err != nil {
		return nil, err
	}
	for _, p := range profiles {
		for _, bound := range p.ViewGroups.V {
			if bound == id {
				refs = append(refs, "组合订阅「"+p.Name+"」")
				break
			}
		}
	}
	return refs, nil
}

// validateViewGroupBinding checks an ordered binding list (plan/profile
// side): size cap, no duplicates, every id exists.
func (s *Server) validateViewGroupBinding(ids []string) error {
	if len(ids) > maxViewGroupsPerBinding {
		return fmt.Errorf("最多绑定 %d 个视图组", maxViewGroupsPerBinding)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("视图组 %s 被重复绑定", id)
		}
		seen[id] = true
	}
	if len(ids) == 0 {
		return nil
	}
	var count int64
	if err := s.st.DB.Model(&store.SubViewGroup{}).Where("id IN ?", ids).Count(&count).Error; err != nil {
		return err
	}
	if count != int64(len(ids)) {
		return errors.New("绑定的视图组中有不存在的 id")
	}
	return nil
}

// --- CRUD ---

func (s *Server) listSubViewGroups(c *gin.Context) {
	var gs []store.SubViewGroup
	if err := s.st.DB.Order("pinned DESC, sort_order ASC, name ASC").Find(&gs).Error; err != nil {
		serverError(c, err)
		return
	}
	if gs == nil {
		gs = []store.SubViewGroup{}
	}
	c.JSON(http.StatusOK, gs)
}

type viewGroupReq struct {
	Name             string                   `json:"name"`
	Remark           string                   `json:"remark"`
	Selector         sub.Operator             `json:"selector"`
	ProcessingGroups []store.SubProcessingRef `json:"processingGroups"`
	Operators        []sub.Operator           `json:"operators"`
	Kinds            []string                 `json:"kinds"`
	ChainIDs         []string                 `json:"chainIds"`
	Icon             string                   `json:"icon"`
	SortOrder        int                      `json:"sortOrder"`
	Pinned           bool                     `json:"pinned"`
	Grouped          bool                     `json:"grouped"`
	Enabled          *bool                    `json:"enabled"`
	Revision         int64                    `json:"revision"`
}

func (r *viewGroupReq) applyTo(vg *store.SubViewGroup) {
	vg.Name, vg.Remark = r.Name, r.Remark
	vg.Selector = store.NewJSON(r.Selector)
	vg.ProcessingGroups = store.NewJSON(r.ProcessingGroups)
	vg.Operators = store.NewJSON(r.Operators)
	vg.Kinds = store.NewJSON(r.Kinds)
	vg.ChainIDs = store.NewJSON(r.ChainIDs)
	vg.Icon, vg.SortOrder, vg.Pinned, vg.Grouped = r.Icon, r.SortOrder, r.Pinned, r.Grouped
	if r.Enabled != nil {
		vg.Enabled = *r.Enabled
	}
}

func (s *Server) createSubViewGroup(c *gin.Context) {
	var req viewGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	vg := store.SubViewGroup{ID: store.NewID(), Revision: 1, Enabled: true}
	req.applyTo(&vg)
	if err := s.validateViewGroup(&vg); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.st.DB.Create(&vg).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "sub_view_group.create", vg.Name, fmt.Sprintf("rev %d", vg.Revision))
	c.JSON(http.StatusCreated, vg)
}

func (s *Server) updateSubViewGroup(c *gin.Context) {
	var cur store.SubViewGroup
	if err := s.st.DB.First(&cur, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req viewGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	vg := cur
	req.applyTo(&vg)
	if err := s.validateViewGroup(&vg); err != nil {
		badRequest(c, err)
		return
	}
	res := s.st.DB.Model(&store.SubViewGroup{}).
		Where("id = ? AND revision = ?", cur.ID, req.Revision).
		Updates(map[string]any{
			"name": vg.Name, "remark": vg.Remark, "selector": vg.Selector,
			"processing_groups": vg.ProcessingGroups, "operators": vg.Operators,
			"kinds": vg.Kinds, "chain_ids": vg.ChainIDs, "icon": vg.Icon,
			"sort_order": vg.SortOrder, "pinned": vg.Pinned, "grouped": vg.Grouped,
			"enabled": vg.Enabled, "revision": gorm.Expr("revision + 1"),
		})
	if res.Error != nil {
		dbError(c, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "视图组已被修改或不存在，请刷新后重试"})
		return
	}
	var out store.SubViewGroup
	s.st.DB.First(&out, "id = ?", cur.ID)
	s.auditCtx(c, "sub_view_group.update", out.Name, fmt.Sprintf("rev %d", out.Revision))
	c.JSON(http.StatusOK, out)
}

func (s *Server) deleteSubViewGroup(c *gin.Context) {
	var vg store.SubViewGroup
	if err := s.st.DB.First(&vg, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	refs, err := s.viewGroupReferences(vg.ID)
	if err != nil {
		serverError(c, err)
		return
	}
	if len(refs) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "视图组仍被绑定，解除绑定后才能删除", "references": refs})
		return
	}
	if err := s.st.DB.Delete(&vg).Error; err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "sub_view_group.delete", vg.Name, "")
	c.Status(http.StatusNoContent)
}

// --- pipeline ---

// resolvedViewGroup is a view-group definition with its full operator chain
// resolved: referenced processing groups (in order) followed by local
// operators.
type resolvedViewGroup struct {
	def   store.SubViewGroup
	chain []sub.Operator
}

// loadViewGroupDefs batch-loads the enabled definitions for ids (one query)
// plus every referenced processing group (one query) and any pinned
// versions (one query) — a constant number of queries regardless of node
// count. Missing/disabled groups and groups with a dangling processing
// reference are skipped: their nodes fall through to the default group
// instead of breaking the subscription.
func (s *Server) loadViewGroupDefs(ids []string) ([]resolvedViewGroup, error) {
	var defs []store.SubViewGroup
	if err := s.st.DB.Where("id IN ? AND enabled = ?", ids, true).Find(&defs).Error; err != nil {
		return nil, err
	}
	byID := make(map[string]store.SubViewGroup, len(defs))
	refIDs := map[string]bool{}
	var pinnedIDs []string
	for _, d := range defs {
		byID[d.ID] = d
		for _, r := range d.ProcessingGroups.V {
			if !refIDs[r.GroupID] {
				refIDs[r.GroupID] = true
			}
			if r.Revision > 0 {
				pinnedIDs = append(pinnedIDs, r.GroupID)
			}
		}
	}
	pgByID := map[string]store.SubProcessingGroup{}
	verByKey := map[string]store.SubProcessingGroupVersion{}
	if len(refIDs) > 0 {
		keys := make([]string, 0, len(refIDs))
		for id := range refIDs {
			keys = append(keys, id)
		}
		var groups []store.SubProcessingGroup
		if err := s.st.DB.Where("id IN ?", keys).Find(&groups).Error; err != nil {
			return nil, err
		}
		for _, g := range groups {
			pgByID[g.ID] = g
		}
		if len(pinnedIDs) > 0 {
			var versions []store.SubProcessingGroupVersion
			if err := s.st.DB.Where("group_id IN ?", pinnedIDs).Find(&versions).Error; err != nil {
				return nil, err
			}
			for _, v := range versions {
				verByKey[fmt.Sprintf("%s@%d", v.GroupID, v.Revision)] = v
			}
		}
	}
	out := make([]resolvedViewGroup, 0, len(defs))
	for _, id := range ids {
		d, ok := byID[id]
		if !ok {
			continue
		}
		chain := []sub.Operator{}
		dangling := false
		for _, r := range d.ProcessingGroups.V {
			if r.Revision > 0 {
				v, ok := verByKey[fmt.Sprintf("%s@%d", r.GroupID, r.Revision)]
				if !ok {
					dangling = true
					break
				}
				chain = append(chain, v.Operators.V...)
			} else {
				g, ok := pgByID[r.GroupID]
				if !ok {
					dangling = true
					break
				}
				chain = append(chain, g.Operators.V...)
			}
		}
		if dangling {
			s.log.Warn("view group skipped: dangling processing-group reference", "group", d.Name)
			continue
		}
		chain = append(chain, d.Operators.V...)
		out = append(out, resolvedViewGroup{def: d, chain: chain})
	}
	return out, nil
}

// applyViewGroups runs each group over the nodes it selects from the
// remaining pool, in binding order, then appends whatever no group claimed
// (the default group) untouched. Selection only ever subtracts from the
// authorised set; a node claimed by a group but dropped by that group's own
// chain is gone, exactly as with the global operator chain.
func applyViewGroups(nodes []sub.Node, groups []resolvedViewGroup) ([]sub.Node, error) {
	if len(groups) == 0 {
		return nodes, nil
	}
	remaining := nodes
	out := make([]sub.Node, 0, len(nodes))
	for _, g := range groups {
		pred := g.def.Selector.V
		pred.Type = "filter"
		pred.Disabled = false
		if len(g.def.Kinds.V) > 0 {
			pred.Kinds = g.def.Kinds.V
		}
		if len(g.def.ChainIDs.V) > 0 {
			pred.ChainIDs = g.def.ChainIDs.V
		}
		matched, rest, err := sub.SplitByFilter(remaining, pred)
		if err != nil {
			return nil, err
		}
		remaining = rest
		for i := range matched {
			matched[i].PanelViewGroup = g.def.Name
		}
		processed, err := sub.ApplyChain(matched, g.chain)
		if err != nil {
			return nil, fmt.Errorf("view group %s: %w", g.def.Name, err)
		}
		out = append(out, processed...)
	}
	out = append(out, remaining...)
	return sub.Dedupe(out), nil
}

// applyPlanViewGroups runs the view groups bound to u's plan over nodes.
// Users without a plan or without a binding get their nodes back untouched;
// the only added cost then is the single plan read.
func (s *Server) applyPlanViewGroups(u *store.User, nodes []sub.Node) ([]sub.Node, error) {
	if u == nil || u.PlanID == "" || len(nodes) == 0 {
		return nodes, nil
	}
	var plan store.Group
	if err := s.st.DB.Select("view_groups").First(&plan, "id = ? AND plan = ?", u.PlanID, true).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nodes, nil
		}
		return nil, err
	}
	ids := plan.ViewGroups.V
	if len(ids) == 0 {
		return nodes, nil
	}
	groups, err := s.loadViewGroupDefs(ids)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nodes, nil
	}
	return applyViewGroups(nodes, groups)
}
