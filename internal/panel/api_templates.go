package panel

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func (s *Server) listTemplates(c *gin.Context) {
	var ts []store.InboundTemplate
	s.st.DB.Order("builtin desc, name").Find(&ts)
	type view struct {
		store.InboundTemplate
		Inbounds int64 `json:"inbounds"`
		Nodes    int64 `json:"nodes"`
	}
	out := make([]view, 0, len(ts))
	for _, t := range ts {
		v := view{InboundTemplate: t}
		s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", t.ID).Count(&v.Inbounds)
		s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", t.ID).Distinct("node_id").Count(&v.Nodes)
		out = append(out, v)
	}
	c.JSON(http.StatusOK, out)
}

type templateReq struct {
	Name        string               `json:"name" binding:"required"`
	Description string               `json:"description"`
	Items       []store.TemplateItem `json:"items" binding:"required"`
	Groups      []string             `json:"groups"`
	BoundLabels []string             `json:"boundLabels"`
}

func (s *Server) createTemplate(c *gin.Context) {
	var req templateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	t := store.InboundTemplate{ID: store.NewID(), Name: req.Name, Description: req.Description, Items: store.NewJSON(req.Items),
		Groups: store.NewJSON(nonNilStrs(req.Groups)), BoundLabels: store.NewJSON(nonNilStrs(req.BoundLabels))}
	if err := s.st.DB.Create(&t).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "template.create", t.Name, "")
	c.JSON(http.StatusCreated, t)
}

func (s *Server) updateTemplate(c *gin.Context) {
	var t store.InboundTemplate
	if err := s.st.DB.First(&t, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req templateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	t.Name, t.Description, t.Items = req.Name, req.Description, store.NewJSON(req.Items)
	t.Groups, t.BoundLabels = store.NewJSON(nonNilStrs(req.Groups)), store.NewJSON(nonNilStrs(req.BoundLabels))
	if err := s.st.DB.Save(&t).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "template.update", t.Name, "")
	c.JSON(http.StatusOK, t)
}

func (s *Server) deleteTemplate(c *gin.Context) {
	var t store.InboundTemplate
	if err := s.st.DB.First(&t, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	// Generated inbounds stay, detached from the template.
	s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", t.ID).Update("template_id", "")
	s.st.DB.Delete(&t)
	s.auditCtx(c, "template.delete", t.Name, "")
	c.Status(http.StatusNoContent)
}

// --- generation engine ---

type planOptions struct {
	NodeIDs      []string `json:"nodeIds"`
	Labels       []string `json:"labels"`       // alternatively select nodes by label
	Reassign     bool     `json:"reassign"`     // move to the node's other core when unsupported
	AutoPort     bool     `json:"autoPort"`     // pick another port on conflict
	BindLabel    string   `json:"bindLabel"`    // bind the template to a node label
	ItemKeys     []string `json:"itemKeys"`     // restrict to some items
	CDNProfileID string   `json:"cdnProfileId"` // used by cdn endpoint items without a profile
}

// PlannedInbound is one row of a generation preview.
type PlannedInbound struct {
	NodeID   string         `json:"nodeId"`
	NodeName string         `json:"nodeName"`
	ItemKey  string         `json:"itemKey"`
	Tag      string         `json:"tag"`
	Core     model.CoreType `json:"core,omitempty"`
	Port     int            `json:"port,omitempty"`
	Network  string         `json:"network,omitempty"`
	Cert     string         `json:"cert"`
	Entries  string         `json:"entries"`
	Result   string         `json:"result"` // new / fix / skip
	Reason   string         `json:"reason,omitempty"`

	inbound *model.Inbound
	item    store.TemplateItem
}

type nodePlan struct {
	NodeID   string           `json:"nodeId"`
	NodeName string           `json:"nodeName"`
	Summary  string           `json:"summary"`
	Items    []PlannedInbound `json:"items"`
	Error    string           `json:"error,omitempty"`
}

var tagSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_.\-]+`)

// expandVars substitutes template variables; random values are per node.
func expandVars(s string, node *store.Node, proto string, rnd map[string]string) string {
	if !strings.Contains(s, "{") {
		return s
	}
	r := strings.NewReplacer(
		"{node.name}", node.Name, "{node.region}", node.Region, "{node.domain}", node.Domain,
		"{node.ip}", node.Address, "{protocol}", proto,
		"{rand.path}", rnd["path"], "{rand.shortid}", rnd["shortid"], "{rand.port}", rnd["port"],
	)
	return r.Replace(s)
}

func (s *Server) selectNodes(opt *planOptions) ([]store.Node, error) {
	var ns []store.Node
	q := s.st.DB.Order("sort_order, name")
	if len(opt.NodeIDs) > 0 {
		q = q.Where("id IN ?", opt.NodeIDs)
	}
	if err := q.Find(&ns).Error; err != nil {
		return nil, err
	}
	if len(opt.Labels) > 0 {
		var out []store.Node
		for _, n := range ns {
			if hasAny(n.Labels.V, opt.Labels) {
				out = append(out, n)
			}
		}
		ns = out
	}
	return ns, nil
}

func hasAny(have, want []string) bool {
	for _, a := range have {
		for _, b := range want {
			if a == b {
				return true
			}
		}
	}
	return false
}

// plan computes, without writing, what applying a template would do.
func (s *Server) plan(t *store.InboundTemplate, opt *planOptions, issueCerts bool) ([]nodePlan, error) {
	nodes, err := s.selectNodes(opt)
	if err != nil {
		return nil, err
	}
	var out []nodePlan
	for i := range nodes {
		out = append(out, s.planNode(t, &nodes[i], opt, issueCerts))
	}
	return out, nil
}

func (s *Server) planNode(t *store.InboundTemplate, node *store.Node, opt *planOptions, issueCerts bool) nodePlan {
	np := nodePlan{NodeID: node.ID, NodeName: node.Name}
	cores, _ := s.nodeCores(node.ID)
	var coreNames []string
	for _, cr := range cores {
		coreNames = append(coreNames, string(cr.Type))
	}
	np.Summary = "已装 " + strings.Join(coreNames, " + ")
	if len(cores) == 0 {
		np.Summary = "未安装内核"
	}
	used, _ := s.usedPorts(node.ID)
	var existingTags []string
	s.st.DB.Model(&store.Inbound{}).Where("node_id = ?", node.ID).Pluck("tag", &existingTags)
	tagUsed := map[string]bool{}
	for _, tg := range existingTags {
		tagUsed[tg] = true
	}
	if used[443] {
		np.Summary += " · 443 已被占用"
	}
	for _, item := range t.Items.V {
		if len(opt.ItemKeys) > 0 && !contains(opt.ItemKeys, item.Key) {
			continue
		}
		rnd := map[string]string{"path": pki.Hex(6), "shortid": pki.ShortID(8)}
		proto := string(item.Protocol.Type)
		if item.Transport.Type != "" && item.Transport.Type != model.TransTCP && item.Transport.Type != model.TransQUIC {
			proto += "-" + string(item.Transport.Type)
		}
		tag := tagSanitizer.ReplaceAllString(expandVars(orDefault(item.TagPattern, "{node.name}-{protocol}"), node, proto, rnd), "-")
		pi := PlannedInbound{NodeID: node.ID, NodeName: node.Name, ItemKey: item.Key, Tag: tag, item: item, Result: "new"}
		if tagUsed[tag] {
			pi.Result, pi.Reason = "skip", "已存在同名入站"
			np.Items = append(np.Items, pi)
			continue
		}
		m := model.Inbound{NodeID: node.ID, Tag: tag, Core: item.Core, Protocol: item.Protocol, Transport: item.Transport,
			Security: item.Security, Sniffing: item.Sniffing, Enabled: true}
		m.Transport.Path = expandVars(m.Transport.Path, node, proto, rnd)
		m.Transport.Host = expandVars(m.Transport.Host, node, proto, rnd)
		m.Security.ServerName = expandVars(m.Security.ServerName, node, proto, rnd)
		if m.Security.ServerName == "" && m.Security.Type == model.SecTLS {
			m.Security.ServerName = orDefault(node.Domain, node.Address)
		}
		for i, sid := range m.Security.RealityShortIDs {
			m.Security.RealityShortIDs[i] = expandVars(sid, node, proto, rnd)
		}
		// Per-node secrets: never share keys between nodes.
		m.Security.RealityPrivateKey, m.Security.RealityPublicKey = "", ""
		m.Protocol.ServerKey = ""
		m.Port = 1 // placeholder until the port policy runs below
		m.Normalize()
		if err := s.prepareInbound(&m); err != nil {
			pi.Result, pi.Reason = "skip", err.Error()
			np.Items = append(np.Items, pi)
			continue
		}
		// Core assignment, with optional reassignment.
		want := item.Core
		if err := s.assignCore(node, &m); err != nil {
			if opt.Reassign && want != model.CoreAuto && want != "" {
				m.Core = model.CoreAuto
				if err2 := s.assignCore(node, &m); err2 == nil {
					pi.Result, pi.Reason = "fix", "改派 "+string(m.Core)
				} else {
					pi.Result, pi.Reason = "skip", shortReason(item, cores)
				}
			} else {
				pi.Result, pi.Reason = "skip", shortReason(item, cores)
			}
			if pi.Result == "skip" {
				np.Items = append(np.Items, pi)
				continue
			}
		} else if want != "" && want != model.CoreAuto && want != m.Core {
			pi.Result, pi.Reason = "fix", "改派 "+string(m.Core)
		}
		// Port policy.
		port := 0
		switch item.Port.Mode {
		case "fixed":
			port = item.Port.Fixed
		default:
			lo, hi := item.Port.Min, item.Port.Max
			if lo <= 0 || hi < lo {
				lo, hi = 20000, 30000
			}
			for tries := 0; tries < 200; tries++ {
				p := lo + rand.IntN(hi-lo+1)
				if !used[p] {
					port = p
					break
				}
			}
		}
		m.Port = port
		if port <= 0 || s.portConflict(node.ID, &m, "") != nil || used[port] {
			if !opt.AutoPort && item.Port.Mode == "fixed" {
				pi.Result, pi.Reason = "skip", fmt.Sprintf("端口 %d 已被占用", port)
				np.Items = append(np.Items, pi)
				continue
			}
			alt := s.findFreePort(node.ID, &m, used, item.Port.Fixed)
			if alt == 0 {
				pi.Result, pi.Reason = "skip", "没有可用端口"
				np.Items = append(np.Items, pi)
				continue
			}
			if item.Port.Mode == "fixed" {
				pi.Result, pi.Reason = "fix", fmt.Sprintf("端口 %d → %d", port, alt)
			}
			m.Port = alt
		}
		if strings.Contains(item.TagPattern, "{rand.port}") {
			// The port is only known now.
			rnd["port"] = strconv.Itoa(m.Port)
			tag = tagSanitizer.ReplaceAllString(expandVars(item.TagPattern, node, proto, rnd), "-")
			m.Tag, pi.Tag = tag, tag
			if tagUsed[tag] {
				pi.Result, pi.Reason = "skip", "已存在同名入站"
				np.Items = append(np.Items, pi)
				continue
			}
		}
		used[m.Port] = true
		tagUsed[tag] = true
		// Certificates.
		switch {
		case m.Security.Type == model.SecReality:
			pi.Cert = "新密钥对"
		case m.Security.Type == model.SecTLS:
			domain := m.Security.ServerName
			if issueCerts {
				cert, label, err := s.certForPolicy(item.Cert, domain)
				if err != nil {
					pi.Result, pi.Reason = "skip", "证书："+err.Error()
					np.Items = append(np.Items, pi)
					continue
				}
				m.Security.CertificateID, pi.Cert = cert.ID, label
			} else {
				switch {
				case item.Cert == "acme" && s.acmeConfigured():
					pi.Cert = "ACME"
				case item.Cert == "acme":
					pi.Cert = "自签（未配置 ACME）"
				case strings.HasPrefix(item.Cert, "wildcard:"):
					pi.Cert = "泛域名证书"
				default:
					pi.Cert = "自签"
				}
			}
		default:
			pi.Cert = "—"
		}
		var entries []string
		for _, e := range item.Endpoints {
			switch e.Kind {
			case "direct":
				entries = append(entries, "直连")
			case "cdn":
				if m.CDNCapable() {
					entries = append(entries, "CDN")
				}
			case "relay":
				entries = append(entries, "中转")
			}
		}
		if len(entries) == 0 {
			entries = []string{"直连"}
		}
		pi.Entries = strings.Join(entries, " + ")
		pi.Core, pi.Port, pi.Network = m.Core, m.Port, m.Network()
		mm := m
		pi.inbound = &mm
		np.Items = append(np.Items, pi)
	}
	return np
}

func shortReason(item store.TemplateItem, cores []store.Core) string {
	if item.Transport.Type == model.TransXHTTP {
		return "XHTTP 仅 Xray"
	}
	for _, cr := range cores {
		if caps, ok := core.Capabilities(cr.Type, cr.Version); ok && caps.SupportsProtocol(item.Protocol.Type) {
			return "组合不受支持"
		}
	}
	return fmt.Sprintf("%s 不受已装内核支持", item.Protocol.Type)
}

func (s *Server) findFreePort(nodeID string, m *model.Inbound, used map[int]bool, near int) int {
	cands := []int{}
	if near == 443 || near == 2053 {
		cands = append(cands, 8443, 2053, 2083, 2087, 2096, 443)
	}
	for p := 20000; p < 30000; p += 1 + rand.IntN(97) {
		cands = append(cands, p)
	}
	for _, p := range cands {
		if used[p] {
			continue
		}
		x := *m
		x.Port = p
		if s.portConflict(nodeID, &x, "") == nil {
			return p
		}
	}
	return 0
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func (s *Server) previewTemplate(c *gin.Context) {
	var t store.InboundTemplate
	if err := s.st.DB.First(&t, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var opt planOptions
	if err := c.ShouldBindJSON(&opt); err != nil {
		badRequest(c, err)
		return
	}
	plans, err := s.plan(&t, &opt, false)
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"plans": plans, "summary": summarize(plans)})
}

func summarize(plans []nodePlan) gin.H {
	nNew, nFix, nSkip := 0, 0, 0
	for _, p := range plans {
		for _, it := range p.Items {
			switch it.Result {
			case "new":
				nNew++
			case "fix":
				nFix++
			case "skip":
				nSkip++
			}
		}
	}
	return gin.H{"nodes": len(plans), "create": nNew + nFix, "adjusted": nFix, "skipped": nSkip}
}

// applyTemplate writes the plan node by node; one node failing does not
// affect the others.
func (s *Server) applyTemplate(c *gin.Context) {
	var t store.InboundTemplate
	if err := s.st.DB.First(&t, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var opt planOptions
	if err := c.ShouldBindJSON(&opt); err != nil {
		badRequest(c, err)
		return
	}
	plans, err := s.applyPlan(&t, &opt)
	if err != nil {
		serverError(c, err)
		return
	}
	if opt.BindLabel != "" && !contains(t.BoundLabels.V, opt.BindLabel) {
		t.BoundLabels.V = append(t.BoundLabels.V, opt.BindLabel)
		s.st.DB.Model(&t).Update("bound_labels", t.BoundLabels)
	}
	s.auditCtx(c, "template.apply", t.Name, fmt.Sprintf("%v", summarize(plans)))
	c.JSON(http.StatusOK, gin.H{"plans": plans, "summary": summarize(plans)})
}

func (s *Server) applyPlan(t *store.InboundTemplate, opt *planOptions) ([]nodePlan, error) {
	nodes, err := s.selectNodes(opt)
	if err != nil {
		return nil, err
	}
	var plans []nodePlan
	defGroups := s.defaultGroupIDs() // outside the transactions (single SQLite connection)
	for i := range nodes {
		np := s.planNode(t, &nodes[i], opt, true)
		err := s.st.DB.Transaction(func(tx *gorm.DB) error {
			for j := range np.Items {
				it := &np.Items[j]
				if it.Result == "skip" || it.inbound == nil {
					continue
				}
				row := store.Inbound{ID: store.NewID(), TemplateID: t.ID, TemplateItemKey: it.ItemKey, OverriddenFields: store.NewJSON([]string{})}
				fromModel(it.inbound, &row)
				s.sealInbound(&row)
				if err := tx.Create(&row).Error; err != nil {
					return err
				}
				eps := it.item.Endpoints
				if len(eps) == 0 {
					eps = []store.EndpointTemplate{{Kind: "direct"}}
				}
				for _, e := range eps {
					ep := store.Endpoint{ID: store.NewID(), InboundID: row.ID, Kind: e.Kind, Port: e.Port,
						Address: expandVars(e.Address, &nodes[i], "", nil)}
					switch e.Kind {
					case "cdn":
						ep.CDNProfileID = orDefault(e.CDNProfileID, opt.CDNProfileID)
						if ep.CDNProfileID == "" || !it.inbound.CDNCapable() {
							continue
						}
					case "relay":
						if e.RelayChainID == "" {
							continue
						}
						ep.RelayChainID = e.RelayChainID
					}
					if err := tx.Create(&ep).Error; err != nil {
						return err
					}
				}
				for _, g := range groupsOr(t.Groups.V, defGroups) {
					if err := tx.Create(&store.GroupInbound{GroupID: g, InboundID: row.ID}).Error; err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			np.Error = err.Error()
			for j := range np.Items {
				if np.Items[j].Result != "skip" {
					np.Items[j].Result, np.Items[j].Reason = "skip", "写入失败"
				}
			}
		} else {
			s.MarkDirty("template "+t.Name, nodes[i].ID)
		}
		plans = append(plans, np)
	}
	return plans, nil
}

// syncTemplate shows (confirm=false) or applies (confirm=true) template
// changes to generated inbounds, keeping fields marked as overridden.
func (s *Server) syncTemplate(c *gin.Context) {
	var t store.InboundTemplate
	if err := s.st.DB.First(&t, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req struct {
		Confirm   bool `json:"confirm"`
		Overwrite bool `json:"overwrite"` // also overwrite overridden fields
	}
	_ = c.ShouldBindJSON(&req)
	var ins []store.Inbound
	s.st.DB.Where("template_id = ?", t.ID).Find(&ins)
	items := map[string]store.TemplateItem{}
	for _, it := range t.Items.V {
		items[it.Key] = it
	}
	type change struct {
		InboundID string   `json:"inboundId"`
		Tag       string   `json:"tag"`
		NodeName  string   `json:"nodeName"`
		Fields    []string `json:"fields"`
		Kept      []string `json:"kept"`
		Error     string   `json:"error,omitempty"`
	}
	names := s.nodeNames()
	var changes []change
	dirty := map[string]bool{}
	for _, in := range ins {
		item, ok := matchItem(items, &in)
		if !ok {
			continue
		}
		cur := s.toModelInbound(&in)
		next := cur
		over := map[string]bool{}
		for _, f := range in.OverriddenFields.V {
			over[f] = !req.Overwrite
		}
		var fields, kept []string
		apply := func(name string, eq bool, set func()) {
			if eq {
				return
			}
			if over[name] {
				kept = append(kept, name)
				return
			}
			fields = append(fields, name)
			set()
		}
		var node store.Node
		s.st.DB.First(&node, "id = ?", in.NodeID)
		wantProto := item.Protocol
		wantProto.ServerKey = cur.Protocol.ServerKey
		if wantProto.Type == model.ProtoHysteria2 && wantProto.Obfs != "" && wantProto.ObfsPassword == "" {
			wantProto.ObfsPassword = cur.Protocol.ObfsPassword
		}
		wantT := item.Transport
		if strings.Contains(wantT.Path, "{rand.path}") || wantT.Path == "" {
			wantT.Path = cur.Transport.Path // random values are kept per node
		} else {
			wantT.Path = expandVars(wantT.Path, &node, "", nil)
		}
		wantT.Host = expandVars(wantT.Host, &node, "", nil)
		if wantT.ServiceName == "" || strings.Contains(wantT.ServiceName, "{rand.") {
			wantT.ServiceName = cur.Transport.ServiceName
		} else {
			wantT.ServiceName = expandVars(wantT.ServiceName, &node, "", nil)
		}
		wantS := item.Security
		wantS.ServerName = expandVars(wantS.ServerName, &node, "", nil)
		if wantS.ServerName == "" {
			wantS.ServerName = cur.Security.ServerName
		}
		wantS.CertificateID, wantS.RealityPrivateKey, wantS.RealityPublicKey = cur.Security.CertificateID, cur.Security.RealityPrivateKey, cur.Security.RealityPublicKey
		if len(wantS.RealityShortIDs) == 0 || strings.Contains(strings.Join(wantS.RealityShortIDs, ""), "{") {
			wantS.RealityShortIDs = cur.Security.RealityShortIDs
		}
		// Compare against the template run through the same defaults the
		// stored inbound went through (sniffing overrides, SS method, …),
		// or every sync would report changes.
		want := cur
		want.Protocol, want.Transport, want.Security, want.Sniffing = wantProto, wantT, wantS, item.Sniffing
		_ = s.prepareInbound(&want)
		wantProto, wantT, wantS = want.Protocol, want.Transport, want.Security
		apply("protocol", jsonEq(cur.Protocol, wantProto), func() { next.Protocol = wantProto })
		apply("transport", jsonEq(cur.Transport, wantT), func() { next.Transport = wantT })
		cs, ws := cur.Security, wantS
		cs.CertPEM, cs.KeyPEM = "", ""
		ws.CertPEM, ws.KeyPEM = "", ""
		apply("security", jsonEq(cs, ws), func() { next.Security = wantS })
		apply("sniffing", jsonEq(cur.Sniffing, want.Sniffing), func() { next.Sniffing = want.Sniffing })
		if item.Port.Mode == "fixed" {
			apply("port", cur.Port == item.Port.Fixed, func() { next.Port = item.Port.Fixed })
		}
		if len(fields) == 0 {
			if len(kept) > 0 {
				changes = append(changes, change{InboundID: in.ID, Tag: in.Tag, NodeName: names[in.NodeID], Kept: kept})
			}
			continue
		}
		ch := change{InboundID: in.ID, Tag: in.Tag, NodeName: names[in.NodeID], Fields: fields, Kept: kept}
		if req.Confirm {
			if err := s.validateInbound(&node, &next, in.ID); err != nil {
				ch.Error = err.Error()
			} else {
				fromModel(&next, &in)
				if req.Overwrite {
					in.OverriddenFields = store.NewJSON([]string{})
				}
				s.sealInbound(&in)
				if err := s.st.DB.Save(&in).Error; err != nil {
					ch.Error = err.Error()
				} else {
					for _, n := range s.nodesForInbounds(in.ID) {
						dirty[n] = true
					}
				}
			}
		}
		changes = append(changes, ch)
	}
	if req.Confirm {
		var ids []string
		for k := range dirty {
			ids = append(ids, k)
		}
		s.MarkDirty("template sync "+t.Name, ids...)
		s.auditCtx(c, "template.sync", t.Name, fmt.Sprintf("%d inbounds", len(changes)))
	}
	if changes == nil {
		changes = []change{}
	}
	c.JSON(http.StatusOK, gin.H{"changes": changes, "applied": req.Confirm})
}

// matchItem finds the template item that generated an inbound.
// Inbounds generated before the item key was recorded fall back to matching
// protocol and transport, and only when exactly one item matches.
func matchItem(items map[string]store.TemplateItem, in *store.Inbound) (store.TemplateItem, bool) {
	if in.TemplateItemKey != "" {
		it, ok := items[in.TemplateItemKey]
		return it, ok
	}
	var found []store.TemplateItem
	for _, it := range items {
		if it.Protocol.Type == in.Protocol.V.Type && (it.Transport.Type == in.Transport.V.Type ||
			(it.Transport.Type == "" && in.Transport.V.Type == model.TransTCP) ||
			(it.Transport.Type == model.TransQUIC && in.Transport.V.Type == model.TransQUIC)) {
			found = append(found, it)
		}
	}
	if len(found) != 1 {
		return store.TemplateItem{}, false
	}
	return found[0], true
}

func jsonEq(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// bulkTemplateInbounds enables, disables or deletes every generated inbound.
func (s *Server) bulkTemplateInbounds(c *gin.Context) {
	var req struct {
		Action string `json:"action" binding:"required"` // enable / disable / delete
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var ins []store.Inbound
	s.st.DB.Where("template_id = ?", c.Param("id")).Find(&ins)
	ids := make([]string, len(ins))
	for i, in := range ins {
		ids[i] = in.ID
	}
	nodes := s.nodesForInbounds(ids...)
	switch req.Action {
	case "enable", "disable":
		s.st.DB.Model(&store.Inbound{}).Where("id IN ?", ids).Update("enabled", req.Action == "enable")
	case "delete":
		var refs int64
		s.st.DB.Model(&store.Outbound{}).Where("node_ref_inbound_id IN ?", ids).Count(&refs)
		if refs > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "部分入站被链路 / 出站引用"})
			return
		}
		s.st.DB.Where("inbound_id IN ?", ids).Delete(&store.GroupInbound{})
		s.st.DB.Where("kind = ? AND target_id IN ?", "inbound", ids).Delete(&store.UserAccess{})
		s.st.DB.Where("inbound_id IN ?", ids).Delete(&store.Endpoint{})
		s.st.DB.Where("id IN ?", ids).Delete(&store.Inbound{})
	default:
		badRequest(c, fmt.Errorf("unknown action"))
		return
	}
	s.MarkDirty("template bulk "+req.Action, nodes...)
	s.auditCtx(c, "template.bulk", c.Param("id"), fmt.Sprintf("%s %d inbounds", req.Action, len(ids)))
	c.JSON(http.StatusOK, gin.H{"affected": len(ids)})
}

// applyBoundTemplates runs templates bound to the node's labels (called once a
// new node enrols, so "install the agent and it just works").
func (s *Server) applyBoundTemplates(nodeID string) {
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", nodeID).Error; err != nil || len(node.Labels.V) == 0 {
		return
	}
	var ts []store.InboundTemplate
	s.st.DB.Find(&ts)
	for i := range ts {
		if !hasAny(ts[i].BoundLabels.V, node.Labels.V) {
			continue
		}
		plans, err := s.applyPlan(&ts[i], &planOptions{NodeIDs: []string{nodeID}, Reassign: true, AutoPort: true})
		if err != nil {
			s.log.Error("bound template", "template", ts[i].Name, "err", err)
			continue
		}
		s.audit("system", "template.auto_apply", ts[i].Name+" → "+node.Name, fmt.Sprintf("%v", summarize(plans)), "")
	}
}
