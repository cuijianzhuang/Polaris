package panel

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"slices"
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
	ids := make([]string, len(ts))
	for i := range ts {
		ids[i] = ts[i].ID
	}
	inbounds := countBy(s.st.DB, &store.Inbound{}, "template_id", ids)
	nodes := countDistinctBy(s.st.DB, &store.Inbound{}, "template_id", "node_id", ids)
	out := make([]view, 0, len(ts))
	for _, t := range ts {
		out = append(out, view{InboundTemplate: t, Inbounds: inbounds[t.ID], Nodes: nodes[t.ID]})
	}
	c.JSON(http.StatusOK, out)
}

type templateReq struct {
	ipStrategies []bool
	Name         string               `json:"name" binding:"required"`
	Description  string               `json:"description"`
	Items        []store.TemplateItem `json:"items" binding:"required"`
	Groups       []string             `json:"groups"`
	BoundLabels  []string             `json:"boundLabels"`
}

// Keep newly added strategy fields when older template editors omit them.
func (r *templateReq) UnmarshalJSON(data []byte) error {
	type plain templateReq
	if err := json.Unmarshal(data, (*plain)(r)); err != nil {
		return err
	}
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.ipStrategies = make([]bool, len(raw.Items))
	for i, item := range raw.Items {
		_, r.ipStrategies[i] = item["outboundIPStrategy"]
	}
	return nil
}

func (s *Server) createTemplate(c *gin.Context) {
	var req templateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.validateTemplate(&req); err != nil {
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
	for i := range req.Items {
		if i < len(req.ipStrategies) && !req.ipStrategies[i] {
			for _, old := range t.Items.V {
				if old.Key == req.Items[i].Key {
					req.Items[i].OutboundIPStrategy = old.OutboundIPStrategy
					break
				}
			}
		}
	}
	if t.Builtin {
		badRequest(c, fmt.Errorf("内置模板请另存为自定义模板"))
		return
	}
	if err := s.validateTemplate(&req); err != nil {
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
	if t.Builtin {
		badRequest(c, fmt.Errorf("内置模板不能删除"))
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
	PlanID           string                                      `json:"planId,omitempty"`
	NodeIDs          []string                                    `json:"nodeIds"`
	Labels           []string                                    `json:"labels"`       // alternatively select nodes by label
	Reassign         bool                                        `json:"reassign"`     // move to the node's other core when unsupported
	AutoPort         bool                                        `json:"autoPort"`     // pick another port on conflict
	BindLabel        string                                      `json:"bindLabel"`    // bind the template to a node label
	ItemKeys         []string                                    `json:"itemKeys"`     // restrict to some items
	CDNProfileID     string                                      `json:"cdnProfileId"` // used by cdn endpoint items without a profile
	VerifyReality    bool                                        `json:"verifyReality"`
	RealityOverrides map[string]map[string]templateRealityTarget `json:"realityOverrides,omitempty"`
}

type templateRealityTarget struct {
	Dest        string   `json:"dest"`
	ServerNames []string `json:"serverNames"`
}

// PlannedInbound is one row of a generation preview.
type PlannedInbound struct {
	NodeID    string         `json:"nodeId"`
	NodeName  string         `json:"nodeName"`
	ItemKey   string         `json:"itemKey"`
	Tag       string         `json:"tag"`
	Core      model.CoreType `json:"core,omitempty"`
	Port      int            `json:"port,omitempty"`
	Network   string         `json:"network,omitempty"`
	Cert      string         `json:"cert"`
	Entries   string         `json:"entries"`
	Result    string         `json:"result"` // new / fix / skip
	Reason    string         `json:"reason,omitempty"`
	Warnings  []string       `json:"warnings,omitempty"`
	InboundID string         `json:"inboundId,omitempty"`

	inbound *model.Inbound
	item    store.TemplateItem
}

type nodePlan struct {
	NodeID      string           `json:"nodeId"`
	NodeName    string           `json:"nodeName"`
	Summary     string           `json:"summary"`
	Items       []PlannedInbound `json:"items"`
	Error       string           `json:"error,omitempty"`
	ApplyStatus string           `json:"applyStatus,omitempty"`
	Revision    int64            `json:"revision,omitempty"`
}

var tagSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_.\-]+`)

// expandVars substitutes template variables; random values are per node.
func expandVars(s string, node *store.Node, proto string, rnd map[string]string) string {
	if !strings.Contains(s, "{") {
		return s
	}
	pairs := []string{
		"{node.name}", node.Name, "{node.region}", node.Region, "{node.domain}", node.Domain,
		"{node.ip}", node.Address, "{protocol}", proto,
	}
	for _, key := range []string{"path", "shortid", "port"} {
		if value, ok := rnd[key]; ok {
			pairs = append(pairs, "{rand."+key+"}", value)
		}
	}
	return strings.NewReplacer(pairs...).Replace(s)
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
		// Expanding a node's variables must never mutate the template's slices.
		raw, _ := json.Marshal(item)
		var copied store.TemplateItem
		_ = json.Unmarshal(raw, &copied)
		item = copied
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
			Security: item.Security, Sniffing: item.Sniffing, Enabled: true, OutboundIPStrategy: item.OutboundIPStrategy}
		if override, ok := opt.RealityOverrides[node.ID][item.Key]; ok && m.Security.Type == model.SecReality {
			m.Security.RealityDest, m.Security.RealityServerNames = override.Dest, append([]string(nil), override.ServerNames...)
		}
		m.Transport.Path = expandVars(m.Transport.Path, node, proto, rnd)
		m.Transport.Host = expandVars(m.Transport.Host, node, proto, rnd)
		m.Transport.ServiceName = expandVars(m.Transport.ServiceName, node, proto, rnd)
		m.Security.RealityDest = expandVars(m.Security.RealityDest, node, proto, rnd)
		for i, name := range m.Security.RealityServerNames {
			m.Security.RealityServerNames[i] = expandVars(name, node, proto, rnd)
		}
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
		if auth := pki.VLESSAuthOf(item.Protocol.Decryption); auth != "" {
			m.Protocol.Decryption, m.Protocol.Encryption, _ = pki.VLESSEncPair(auth)
		}
		if item.Security.RealityMldsa65Seed != "" {
			m.Security.RealityMldsa65Seed, m.Security.RealityMldsa65Verify, _ = pki.MLDSA65Pair()
		}
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
		if err := s.checkInboundOutboundIP(node, &m); err != nil {
			pi.Result, pi.Reason = "skip", err.Error()
			np.Items = append(np.Items, pi)
			continue
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
			alt := s.findFreePort(node.ID, &m, used, item.Port.Fixed, itemWantsCDN(item))
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
		rnd["port"] = strconv.Itoa(m.Port)
		m.Transport.Path = expandVars(m.Transport.Path, node, proto, rnd)
		m.Transport.Host = expandVars(m.Transport.Host, node, proto, rnd)
		m.Transport.ServiceName = expandVars(m.Transport.ServiceName, node, proto, rnd)
		m.Security.ServerName = expandVars(m.Security.ServerName, node, proto, rnd)
		m.Security.RealityDest = expandVars(m.Security.RealityDest, node, proto, rnd)
		for i, name := range m.Security.RealityServerNames {
			m.Security.RealityServerNames[i] = expandVars(name, node, proto, rnd)
		}
		for i, sid := range m.Security.RealityShortIDs {
			m.Security.RealityShortIDs[i] = expandVars(sid, node, proto, rnd)
		}
		for i := range item.Endpoints {
			item.Endpoints[i].Address = expandVars(item.Endpoints[i].Address, node, proto, rnd)
		}
		pi.item = item
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
				case item.Cert == "acme" && (s.acmeConfigured() || (s.acmeHTTPReady() && s.nodeForDomain(domain) != "")):
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
		cdnWanted, cdnKept := false, false
		for _, e := range item.Endpoints {
			switch e.Kind {
			case "direct":
				entries = append(entries, "直连")
			case "cdn":
				cdnWanted = true
				if !m.CDNCapable() {
					entries = append(entries, "CDN（此组合不能走 CDN，不添加）")
					continue
				}
				id := orDefault(e.CDNProfileID, opt.CDNProfileID)
				var p store.CDNProfile
				if id == "" || s.st.DB.First(&p, "id = ?", id).Error != nil {
					entries = append(entries, "CDN（未选配置档，不添加）")
					continue
				}
				if problem := cdnProblem(&m, &p, e.Port, p.CaddyFronting); problem != "" {
					entries = append(entries, "CDN（不添加）")
					pi.Warnings = append(pi.Warnings, problem)
					continue
				}
				cdnKept = true
				entries = append(entries, "CDN · "+p.Name)
				if w := cdnCertWarning(&m, &p); w != "" {
					pi.Warnings = append(pi.Warnings, w)
				}
				if m.Transport.Type == model.TransGRPC && p.Provider == "cloudflare" {
					pi.Warnings = append(pi.Warnings, "Cloudflare 需要在域名的 Network 设置里打开 gRPC")
				}
			case "relay":
				entries = append(entries, "中转")
			}
		}
		// An item published only through a CDN must not fall back to a
		// direct entry exposing the node's address.
		if cdnWanted && !cdnKept && !slices.ContainsFunc(item.Endpoints, func(e store.EndpointTemplate) bool { return e.Kind != "cdn" }) {
			pi.Result, pi.Reason = "skip", "只走 CDN，但没有可用的 CDN 入口："+strings.Join(entries, "、")
			np.Items = append(np.Items, pi)
			continue
		}
		if len(entries) == 0 {
			entries = []string{"直连"}
		}
		pi.Entries = strings.Join(entries, " + ")
		pi.Core, pi.Port, pi.Network = m.Core, m.Port, m.Network()
		m.ID = store.NewID()
		pi.InboundID = m.ID
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

// An inbound published through a CDN must stay on a port the CDN proxies:
// 443 for gRPC (Cloudflare proxies gRPC only there), else Cloudflare's other
// HTTPS ports first.
func (s *Server) findFreePort(nodeID string, m *model.Inbound, used map[int]bool, near int, cdn bool) int {
	cands := []int{}
	switch {
	case cdn && m.Transport.Type == model.TransGRPC:
		cands = append(cands, 443)
	case cdn || slices.Contains(cloudflareHTTPSPorts, near):
		cands = append(cands, 2053, 2083, 2087, 2096, 8443, 443)
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
	run, plans, err := s.freezeTemplatePlan(&t, &opt)
	if err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"planId": run.ID, "expiresAt": run.ExpiresAt, "plans": plans, "summary": summarize(plans)})
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
	// Older API callers can still request an immediate apply. The web UI
	// always supplies a frozen preview ID and never replans at confirmation.
	if opt.PlanID != "" {
		s.applyFrozenTemplate(c, &t, &opt)
		return
	}
	if opt.VerifyReality {
		badRequest(c, fmt.Errorf("要求节点检测时，请先预览并检测，再提交 planId"))
		return
	}
	plans, err := s.applyPlan(&t, &opt)
	if err != nil {
		serverError(c, err)
		return
	}
	if opt.BindLabel != "" && !contains(t.BoundLabels.V, opt.BindLabel) {
		t.BoundLabels.V = append(t.BoundLabels.V, opt.BindLabel)
		if err := s.st.DB.Model(&t).Update("bound_labels", t.BoundLabels).Error; err != nil {
			s.log.Warn("bind template label", "template", t.Name, "err", err)
		}
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
				if _, ok := opt.RealityOverrides[nodes[i].ID][it.ItemKey]; ok {
					row.OverriddenFields = store.NewJSON([]string{"security.realityDest", "security.realityServerNames"})
				}
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
						if !cdnEndpointUsable(tx, it.inbound, ep.CDNProfileID, ep.Port) {
							continue
						}
						assignCDNHost(tx, &ep, nodes[i].ID)
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
		Confirm      bool                `json:"confirm"`
		Overwrite    bool                `json:"overwrite"` // also overwrite overridden fields
		Selections   map[string][]string `json:"selections"`
		Expected     map[string]string   `json:"expected"`
		TemplateHash string              `json:"templateHash"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var ins []store.Inbound
	s.st.DB.Where("template_id = ?", t.ID).Find(&ins)
	if req.Confirm && req.TemplateHash != "" && req.TemplateHash != templateHash(t) {
		c.JSON(http.StatusConflict, gin.H{"error": "模板已变化，请重新查看同步差异"})
		return
	}
	for _, in := range ins {
		if req.Confirm && req.Expected[in.ID] != "" && req.Expected[in.ID] != templateHash(s.toModelInbound(&in)) {
			c.JSON(http.StatusConflict, gin.H{"error": "入站已变化，请重新查看同步差异"})
			return
		}
	}
	items := map[string]store.TemplateItem{}
	for _, it := range t.Items.V {
		items[it.Key] = it
	}
	type change struct {
		InboundID string                `json:"inboundId"`
		Tag       string                `json:"tag"`
		NodeName  string                `json:"nodeName"`
		Fields    []string              `json:"fields"`
		Kept      []string              `json:"kept"`
		Error     string                `json:"error,omitempty"`
		Details   []templateFieldChange `json:"details"`
		Hash      string                `json:"hash"`
	}
	names := s.nodeNames()
	var changes []change
	dirty := map[string]bool{}
	for _, in := range ins {
		var selected map[string]bool
		if req.Selections != nil {
			if len(req.Selections[in.ID]) == 0 {
				continue
			}
			selected = map[string]bool{}
			for _, field := range req.Selections[in.ID] {
				selected[field] = true
			}
		}
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
		var details []templateFieldChange
		applyGroup := func(name string, before, after, destination any) {
			f, k, d := mergeTemplateGroup(name, before, after, destination, over, selected)
			fields, kept, details = append(fields, f...), append(kept, k...), append(details, d...)
		}
		var node store.Node
		s.st.DB.First(&node, "id = ?", in.NodeID)
		expand := func(pattern, previous string) string {
			if strings.Contains(pattern, "{rand.path}") || strings.Contains(pattern, "{rand.shortid}") {
				return previous
			}
			proto := string(item.Protocol.Type)
			if item.Transport.Type != "" && item.Transport.Type != model.TransTCP && item.Transport.Type != model.TransQUIC {
				proto += "-" + string(item.Transport.Type)
			}
			return expandVars(pattern, &node, proto, map[string]string{"port": strconv.Itoa(cur.Port)})
		}
		wantProto := item.Protocol
		wantProto.ServerKey = cur.Protocol.ServerKey
		if (wantProto.Decryption != "") == (cur.Protocol.Decryption != "") {
			// Keys are per node: keep this node's, only on/off syncs.
			wantProto.Decryption, wantProto.Encryption = cur.Protocol.Decryption, cur.Protocol.Encryption
		} else if auth := pki.VLESSAuthOf(wantProto.Decryption); auth != "" {
			wantProto.Decryption, wantProto.Encryption, _ = pki.VLESSEncPair(auth)
		}
		if wantProto.Type == model.ProtoHysteria2 && wantProto.Obfs != "" && wantProto.ObfsPassword == "" {
			wantProto.ObfsPassword = cur.Protocol.ObfsPassword
		}
		wantT := item.Transport
		if strings.Contains(wantT.Path, "{rand.path}") || wantT.Path == "" {
			wantT.Path = cur.Transport.Path // random values are kept per node
		} else {
			wantT.Path = expand(wantT.Path, cur.Transport.Path)
		}
		wantT.Host = expand(wantT.Host, cur.Transport.Host)
		if wantT.ServiceName == "" || strings.Contains(wantT.ServiceName, "{rand.") {
			wantT.ServiceName = cur.Transport.ServiceName
		} else {
			wantT.ServiceName = expand(wantT.ServiceName, cur.Transport.ServiceName)
		}
		wantS := item.Security
		wantS.ServerName = expand(wantS.ServerName, cur.Security.ServerName)
		wantS.RealityDest = expand(wantS.RealityDest, cur.Security.RealityDest)
		wantS.RealityServerNames = append([]string(nil), item.Security.RealityServerNames...)
		for i, name := range wantS.RealityServerNames {
			previous := ""
			if i < len(cur.Security.RealityServerNames) {
				previous = cur.Security.RealityServerNames[i]
			}
			wantS.RealityServerNames[i] = expand(name, previous)
		}
		if wantS.ServerName == "" {
			wantS.ServerName = cur.Security.ServerName
		}
		wantS.CertificateID, wantS.RealityPrivateKey, wantS.RealityPublicKey = cur.Security.CertificateID, cur.Security.RealityPrivateKey, cur.Security.RealityPublicKey
		if (wantS.RealityMldsa65Seed != "") == (cur.Security.RealityMldsa65Seed != "") {
			wantS.RealityMldsa65Seed, wantS.RealityMldsa65Verify = cur.Security.RealityMldsa65Seed, cur.Security.RealityMldsa65Verify
		} else if wantS.RealityMldsa65Seed != "" {
			wantS.RealityMldsa65Seed, wantS.RealityMldsa65Verify, _ = pki.MLDSA65Pair()
		}
		if len(wantS.RealityShortIDs) == 0 || strings.Contains(strings.Join(wantS.RealityShortIDs, ""), "{") {
			wantS.RealityShortIDs = cur.Security.RealityShortIDs
		}
		// Compare against the template run through the same defaults the
		// stored inbound went through (sniffing overrides, SS method, …),
		// or every sync would report changes.
		want := cur
		want.Protocol, want.Transport, want.Security, want.Sniffing = wantProto, wantT, wantS, item.Sniffing
		if err := s.prepareInbound(&want); err != nil {
			changes = append(changes, change{InboundID: in.ID, Tag: in.Tag, NodeName: names[in.NodeID], Error: err.Error()})
			continue
		}
		wantProto, wantT, wantS = want.Protocol, want.Transport, want.Security
		next.Protocol = model.ProtocolSpec{}
		next.Transport = model.TransportSpec{}
		applyGroup("protocol", cur.Protocol, wantProto, &next.Protocol)
		applyGroup("transport", cur.Transport, wantT, &next.Transport)
		cs, ws := cur.Security, wantS
		cs.CertPEM, cs.KeyPEM = "", ""
		ws.CertPEM, ws.KeyPEM = "", ""
		next.Security = model.SecuritySpec{}
		next.Sniffing = model.SniffSpec{}
		applyGroup("security", cs, ws, &next.Security)
		applyGroup("sniffing", cur.Sniffing, want.Sniffing, &next.Sniffing)
		if item.Port.Mode == "fixed" && cur.Port != item.Port.Fixed && (selected == nil || selected["port"]) {
			d := templateFieldChange{Field: "port", Before: cur.Port, After: item.Port.Fixed, Kept: over["port"]}
			if d.Kept {
				kept = append(kept, "port")
			} else {
				fields = append(fields, "port")
				next.Port = item.Port.Fixed
			}
			details = append(details, d)
		}
		if cur.OutboundIPStrategy != item.OutboundIPStrategy && (selected == nil || selected["outboundIPStrategy"]) {
			d := templateFieldChange{Field: "outboundIPStrategy", Before: cur.OutboundIPStrategy, After: item.OutboundIPStrategy, Kept: over["outboundIPStrategy"]}
			if d.Kept {
				kept = append(kept, d.Field)
			} else {
				fields = append(fields, d.Field)
				next.OutboundIPStrategy = item.OutboundIPStrategy
			}
			details = append(details, d)
		}
		if len(fields) == 0 {
			if len(kept) > 0 {
				changes = append(changes, change{InboundID: in.ID, Tag: in.Tag, NodeName: names[in.NodeID], Kept: kept, Details: details, Hash: templateHash(cur)})
			}
			continue
		}
		ch := change{InboundID: in.ID, Tag: in.Tag, NodeName: names[in.NodeID], Fields: fields, Kept: kept, Details: details, Hash: templateHash(cur)}
		if req.Confirm {
			if err := s.validateInbound(&node, &next, in.ID); err != nil {
				ch.Error = err.Error()
			} else {
				fromModel(&next, &in)
				if req.Overwrite {
					remaining := []string{}
					for _, field := range in.OverriddenFields.V {
						if req.Selections != nil && !contains(fields, field) {
							remaining = append(remaining, field)
						}
					}
					in.OverriddenFields = store.NewJSON(remaining)
				}
				s.sealInbound(&in)
				result := s.st.DB.Model(&store.Inbound{}).Where("id = ? AND updated_at = ?", in.ID, in.UpdatedAt).Select("*").Updates(&in)
				if result.Error != nil {
					ch.Error = result.Error.Error()
				} else if result.RowsAffected != 1 {
					ch.Error = "入站已变化，请重新查看同步差异"
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
	c.JSON(http.StatusOK, gin.H{"changes": changes, "applied": req.Confirm, "templateHash": templateHash(t)})
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
		if err := s.st.DB.Model(&store.Inbound{}).Where("id IN ?", ids).Update("enabled", req.Action == "enable").Error; err != nil {
			dbError(c, err)
			return
		}
	case "delete":
		var refs int64
		s.st.DB.Model(&store.Outbound{}).Where("node_ref_inbound_id IN ?", ids).Count(&refs)
		if refs > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "部分入站被链路 / 出站引用"})
			return
		}
		// All four or none: a partial delete would leave grants and
		// endpoints pointing at missing inbounds.
		if err := s.st.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Where("inbound_id IN ?", ids).Delete(&store.GroupInbound{}).Error; err != nil {
				return err
			}
			if err := tx.Where("kind = ? AND target_id IN ?", "inbound", ids).Delete(&store.UserAccess{}).Error; err != nil {
				return err
			}
			if err := tx.Where("inbound_id IN ?", ids).Delete(&store.Endpoint{}).Error; err != nil {
				return err
			}
			return tx.Where("id IN ?", ids).Delete(&store.Inbound{}).Error
		}); err != nil {
			dbError(c, err)
			return
		}
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
