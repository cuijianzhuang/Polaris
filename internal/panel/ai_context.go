package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"github.com/gin-gonic/gin"
)

// Deliberately small: no Extra, credentials, destinations, groups or arbitrary
// core JSON can be supplied by a model. Users still review the existing form.
type aiInboundPlan struct {
	Core          model.CoreType  `json:"core"`
	Protocol      model.Protocol  `json:"protocol"`
	Transport     model.Transport `json:"transport"`
	Security      model.Security  `json:"security"`
	Port          int             `json:"port"`
	ServerName    string          `json:"serverName,omitempty"`
	Tag           string          `json:"tag,omitempty"`
	CertificateID string          `json:"certificateId,omitempty"`
}

func (p aiInboundPlan) inbound() model.Inbound {
	m := model.Inbound{ID: "ai-draft", Tag: p.Tag, Port: p.Port, Core: p.Core, Protocol: model.ProtocolSpec{Type: p.Protocol}, Transport: model.TransportSpec{Type: p.Transport}, Security: model.SecuritySpec{Type: p.Security, ServerName: p.ServerName, CertificateID: p.CertificateID}}
	if m.Tag == "" {
		m.Tag = "ai-draft"
	}
	if p.Security == model.SecReality {
		m.Security.ServerName = ""
		m.Security.RealityDest = net.JoinHostPort(p.ServerName, "443")
		m.Security.RealityServerNames = []string{p.ServerName}
	}
	return m
}
func aiInboundSummary(in store.Inbound) gin.H {
	return gin.H{"tag": aiRedact(in.Tag), "core": in.Core, "protocol": in.Protocol.V.Type, "transport": in.Transport.V.Type, "security": in.Security.V.Type, "port": in.Port, "enabled": in.Enabled}
}
func (s *Server) aiNodeContext(req aiRequest) (gin.H, []string, error) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ? AND revoked = ?", req.NodeID, false).Error; err != nil {
		return nil, nil, err
	}
	facts := []string{fmt.Sprintf("节点状态：%s；最后心跳：%v", n.Status, n.LastSeen), fmt.Sprintf("配置版本：期望 %d，已应用 %d", n.DesiredRev, n.AppliedRev)}
	data := gin.H{"status": n.Status, "lastSeen": n.LastSeen, "expectedRevision": n.DesiredRev, "appliedRevision": n.AppliedRev, "lastError": aiRedact(excerpt(n.LastError, 700))}
	data["agentFeatures"] = n.AgentFeatures.V
	cs, err := s.nodeCores(n.ID)
	if err != nil {
		return nil, nil, err
	}
	caps := []model.CapSet{}
	states := []gin.H{}
	for _, cr := range cs {
		if cap, ok := core.Capabilities(cr.Type, cr.Version); ok && len(cap.Combos) > 0 {
			caps = append(caps, cap)
		}
		states = append(states, gin.H{"core": cr.Type, "version": cr.Version, "runningVersion": cr.RunningVersion, "status": cr.Status, "error": aiRedact(excerpt(cr.Error, 500))})
	}
	data["capabilities"], data["cores"] = caps, states
	tlsID, err := s.aiTLSCertificate(n.Domain)
	if err != nil {
		return nil, nil, err
	}
	data["nodeTLSIdentityAvailable"] = tlsID != ""
	var ins []store.Inbound
	if err := s.st.DB.Where("node_id = ?", n.ID).Order("tag,id").Limit(40).Find(&ins).Error; err != nil {
		return nil, nil, err
	}
	views := []gin.H{}
	for _, in := range ins {
		views = append(views, aiInboundSummary(in))
	}
	data["inbounds"] = views
	if req.InboundID != "" {
		var in store.Inbound
		if err := s.st.DB.First(&in, "id = ? AND node_id = ?", req.InboundID, n.ID).Error; err != nil {
			return nil, nil, err
		}
		data["selectedInbound"] = aiInboundSummary(in)
		var health store.InboundHealth
		if s.st.DB.First(&health, "inbound_id = ?", in.ID).Error == nil {
			data["connectionCheck"] = gin.H{"ok": health.OK, "checkedAt": health.CheckedAt, "error": aiRedact(health.Error)}
			facts = append(facts, fmt.Sprintf("服务端连接检查：%t；时间：%s；不等于代理认证与传输验证", health.OK, health.CheckedAt.UTC().Format(time.RFC3339)))
		}
		m := s.toModelInbound(&in)
		for _, check := range []func() error{func() error { return m.Validate() }, func() error { return s.assignCore(&n, &m) }, func() error { return s.portConflict(n.ID, &m, in.ID) }} {
			if err := check(); err != nil {
				facts = append(facts, "配置校验："+aiRedact(err.Error()))
			}
		}
		var es []store.Endpoint
		s.st.DB.Where("inbound_id = ?", in.ID).Limit(16).Find(&es)
		entries := []gin.H{}
		for _, e := range es {
			entry := gin.H{"kind": e.Kind}
			if e.CDNProfileID != "" {
				var p store.CDNProfile
				if s.st.DB.First(&p, "id = ?", e.CDNProfileID).Error == nil {
					entry["cdn"] = gin.H{"scheme": p.Scheme, "ports": p.Ports.V, "originMode": p.OriginMode}
					target := s.cdnTargetFor(&e, &m, &p)
					entry["clientPort"], entry["originPort"] = target.Port, target.OriginPort
					for _, warning := range s.cdnEndpointWarnings(&e, &in, &p) {
						facts = append(facts, "CDN 配置检查："+aiRedact(warning))
					}
				}
			}
			entries = append(entries, entry)
		}
		data["entries"] = entries
	}
	if req.Draft != nil {
		data["proposedInbound"] = req.Draft
		m := req.Draft.inbound()
		if err := s.prepareInbound(&m); err != nil {
			facts = append(facts, "草稿校验："+aiRedact(err.Error()))
		} else if err := s.assignCore(&n, &m); err != nil {
			facts = append(facts, "内核兼容："+aiRedact(err.Error()))
		} else if err := s.portConflict(n.ID, &m, req.InboundID); err != nil {
			facts = append(facts, "端口检查："+aiRedact(err.Error()))
		} else {
			facts = append(facts, "基础结构、当前内核能力和面板内端口检查通过；仍需在保存时检查证书、CDN 与实际下发")
		}
	}
	return data, facts, nil
}
func (s *Server) aiAdminContext(r aiRequest) (gin.H, []string, error) {
	switch r.Mode {
	case "diagnose", "review", "draft":
		if r.NodeID == "" {
			return nil, nil, errors.New("请先选择节点")
		}
		return s.aiNodeContext(r)
	case "summary":
		v, err := s.loadOperationalOverview(time.Now())
		if err != nil {
			return nil, nil, err
		}
		if len(v.Issues) > 24 {
			v.Issues = v.Issues[:24]
			v.Truncated = true
		}
		var tickets []store.Ticket
		if err := s.st.DB.Where("updated_at > ?", time.Now().Add(-24*time.Hour)).Order("updated_at DESC,id").Limit(12).Find(&tickets).Error; err != nil {
			return nil, nil, err
		}
		list := []gin.H{}
		for _, t := range tickets {
			list = append(list, gin.H{"subject": aiRedact(t.Subject), "status": t.Status, "updatedAt": t.UpdatedAt})
		}
		return gin.H{"operations": v, "recentTickets": list}, []string{"运维状态取自本次快照，工单范围为最近 24 小时，最多 12 条"}, nil
	case "ticket":
		var t store.Ticket
		if r.TicketID == "" {
			return nil, nil, errors.New("请选择工单")
		}
		if err := s.st.DB.First(&t, "id = ?", r.TicketID).Error; err != nil {
			return nil, nil, err
		}
		var ms []store.TicketMessage
		if err := s.st.DB.Where("ticket_id = ?", t.ID).Order("created_at DESC,id DESC").Limit(8).Find(&ms).Error; err != nil {
			return nil, nil, err
		}
		slices.Reverse(ms)
		text := []gin.H{}
		for _, m := range ms {
			text = append(text, gin.H{"admin": m.Admin, "body": aiRedact(excerpt(m.Body, 1200))})
		}
		return gin.H{"subject": aiRedact(t.Subject), "status": t.Status, "messages": text}, []string{"工单辅助只生成待审核的回复草稿"}, nil
	default:
		return nil, nil, errors.New("请选择有效的 AI 任务")
	}
}
func (s *Server) aiPortalContext(c *gin.Context, u *store.User) (gin.H, []string, error) {
	// Reuse the subscription authorization/publication path. Never accept a
	// customer-supplied account, ticket, inbound or node ID for retrieval.
	var info sub.Info
	if s.servedUser(u) {
		ns, err := s.subNodesFor(u, s.subCarrier(c.Request, u), s.subRegion(c.Request, u))
		if err != nil {
			return nil, nil, err
		}
		if err = s.portalNodeDetails(&info, ns); err != nil {
			return nil, nil, err
		}
	}
	list := []gin.H{}
	for _, n := range info.Links {
		if len(list) == 32 {
			break
		}
		list = append(list, gin.H{"name": aiRedact(n.Name), "state": n.State, "reason": aiRedact(n.Reason), "network": n.Network, "business": n.Business, "checkedAt": n.CheckedAt})
	}
	return gin.H{"account": gin.H{"status": u.Status, "trafficLimit": u.TrafficLimit, "usedUpload": u.UsedUp, "usedDownload": u.UsedDown, "expiresAt": u.ExpireAt}, "publishedNodes": list}, []string{"仅包含当前登录账号的套餐状态与已发布授权节点，不包含订阅凭据"}, nil
}
func (s *Server) aiGenerate(c *gin.Context) {
	if !aiOwner(c) {
		return
	}
	var r aiRequest
	if !aiReadJSON(c, &r) {
		return
	}
	if err := validateAIRequest(r); err != nil {
		badRequest(c, err)
		return
	}
	if !slices.Contains([]string{"diagnose", "review", "summary", "ticket", "draft"}, r.Mode) || (r.Mode == "ticket" && r.TicketID == "") || (r.Mode != "summary" && r.Mode != "ticket" && r.NodeID == "") {
		badRequest(c, errors.New("请选择有效的任务和对应节点或工单"))
		return
	}
	cfg, err := s.aiConfig()
	if err != nil || !cfg.Enabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "AI 助手尚未开启"})
		return
	}
	data, facts, err := s.aiAdminContext(r)
	if err != nil {
		dbError(c, err)
		return
	}
	if !s.aiAdmit(c, cfg, "admin:"+c.GetString("adminID"), false) {
		return
	}
	s.aiRespond(c, cfg, r, data, facts, false)
}
func (s *Server) aiPortalChat(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	cfg, err := s.aiConfig()
	if err != nil || !cfg.Enabled || !cfg.PortalEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "AI 客服暂未开放"})
		return
	}
	var r aiRequest
	if !aiReadJSON(c, &r) {
		return
	}
	if r.Mode != "" && r.Mode != "support" || r.NodeID != "" || r.InboundID != "" || r.TicketID != "" || r.Draft != nil || r.Logs != "" {
		badRequest(c, errors.New("客服仅支持当前账号的问题"))
		return
	}
	r.Mode = "support"
	if err := validateAIRequest(r); err != nil {
		badRequest(c, err)
		return
	}
	if !s.aiAdmit(c, cfg, "portal:"+u.ID, true) {
		return
	}
	data, facts, err := s.aiPortalContext(c, u)
	if err != nil {
		serverError(c, errors.New("暂时无法读取本人服务状态"))
		return
	}
	s.aiRespond(c, cfg, r, data, facts, true)
}
func (s *Server) aiRespond(c *gin.Context, cfg aiConfig, r aiRequest, data gin.H, facts []string, portal bool) {
	data["collectedAt"] = time.Now().UTC()
	sources := aiKnowledgeFor(r.Prompt+" "+r.Logs+" "+r.Mode, cfg.Knowledge, portal)
	data["facts"], data["knowledge"] = facts, sources
	encoded, err := json.Marshal(data)
	if err != nil {
		serverError(c, err)
		return
	}
	system := aiSystem + "\n引用知识库时标注 [K1] 等编号；没有匹配来源时明确说明。"
	if portal {
		system += "\n你是用户客服，仅讨论本人的套餐、客户端和授权节点。付款、退款、套餐变更与账号解封须由人工按面板实际流程处理。"
	}
	if r.Mode == "draft" {
		system += `\n仅输出一个 JSON 对象，不要代码围栏或说明。格式：{"core":"xray","protocol":"vless","transport":"tcp","security":"reality","port":443,"serverName":"www.microsoft.com"}。只从当前节点 capabilities 选择组合。避开已有端口，禁止提供密钥、Extra、命令或未知字段。TLS 仅在 nodeTLSIdentityAvailable 为 true 时建议，TLS 的 serverName 留空，由面板填入本机证书身份。REALITY 的 serverName 为公开伪装站域名。不确定时优先建议已支持的 REALITY。`
	}
	contextText := aiRedact(string(encoded))
	if len(contextText) > 28000 {
		badRequest(c, errors.New("上下文过大，请缩小选定的节点、日志或知识库范围"))
		return
	}
	messages := []aiMessage{{Role: "user", Content: "服务器采集的只读事实与文档摘录：\n" + contextText}}
	for _, m := range r.History {
		messages = append(messages, aiMessage{Role: m.Role, Content: aiRedact(m.Content)})
	}
	messages = append(messages, aiMessage{Role: "user", Content: "任务：" + r.Mode + "\n问题：" + aiRedact(r.Prompt) + "\n附加日志：" + aiRedact(r.Logs)})
	answer, err := s.askAI(c.Request.Context(), cfg, system, messages)
	if err != nil {
		msg := err.Error()
		if portal {
			msg = "AI 客服暂时不可用，请稍后再试或联系人工客服"
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": msg})
		return
	}
	result := aiAnswer{Text: aiRedact(answer), Facts: facts, CheckedAt: time.Now().UTC(), Sources: sources}
	if portal {
		result.TicketDraft = &aiTicketDraft{Subject: excerpt(aiRedact(r.Prompt), 100), Body: excerpt("用户问题："+aiRedact(r.Prompt)+"\n\nAI 建议（仅供客服参考，尚需核实）：\n"+result.Text, 3500)}
	}
	if r.Mode == "draft" {
		var p aiInboundPlan
		d := json.NewDecoder(strings.NewReader(strings.TrimSpace(answer)))
		d.DisallowUnknownFields()
		if err = d.Decode(&p); err == nil {
			err = d.Decode(new(any))
			if err == io.EOF {
				err = nil
			} else if err == nil {
				err = errors.New("额外 JSON 内容")
			}
		}
		if err == nil {
			p.Tag = string(p.Protocol) + "-" + string(p.Transport) + "-ai-" + pki.Hex(2)
			err = s.checkAIPlan(r.NodeID, &p)
		}
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "AI 草稿未通过本地校验：" + aiRedact(err.Error())})
			return
		}
		result.Draft = &p
		result.Text = "草稿已通过当前内核能力和面板端口检查。请在入站表单中核对证书、CDN 及授权，确认后保存并下发。"
	}
	if !portal {
		s.auditCtx(c, "ai.request", r.Mode, "read-only")
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, result)
}
func (s *Server) checkAIPlan(nodeID string, p *aiInboundPlan) error {
	p.CertificateID = ""
	if !slices.Contains(model.AllProtocols, p.Protocol) || !slices.Contains(model.AllTransports, p.Transport) || !slices.Contains([]model.Security{model.SecNone, model.SecTLS, model.SecReality}, p.Security) {
		return errors.New("协议、传输或安全类型不受支持")
	}
	p.ServerName = strings.ToLower(strings.TrimSuffix(p.ServerName, "."))
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", nodeID).Error; err != nil {
		return err
	}
	if len(p.ServerName) > 253 || strings.ContainsAny(p.ServerName, "/\\\r\n\t\x00 :") {
		return errors.New("SNI 域名格式无效")
	}
	if p.Security == model.SecReality && !aiDomainName(p.ServerName) {
		return errors.New("REALITY 目标须为有效域名")
	}
	if p.Security == model.SecTLS {
		if p.ServerName == "" {
			p.ServerName = n.Domain
		}
		id, err := s.aiTLSCertificate(p.ServerName)
		if err != nil {
			return err
		}
		if id == "" {
			return errors.New("TLS 草稿需要已有且未到期的对应证书，请先配置证书或选择其他安全方式")
		}
		p.CertificateID = id
	}
	if p.Security == model.SecNone {
		p.ServerName = ""
	}
	m := p.inbound()
	if err := s.prepareInbound(&m); err != nil {
		return err
	}
	if err := s.assignCore(&n, &m); err != nil {
		return err
	}
	p.Core = m.Core
	return s.portConflict(nodeID, &m, "")
}

func aiDomainName(name string) bool {
	if net.ParseIP(name) != nil {
		return false
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if ch != '-' && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
				return false
			}
		}
	}
	return true
}

func (s *Server) aiTLSCertificate(host string) (string, error) {
	if host == "" {
		return "", nil
	}
	var certs []store.Certificate
	if err := s.st.DB.Select("id,domain").Where("expire_at > ?", time.Now()).Order("domain,id").Find(&certs).Error; err != nil {
		return "", err
	}
	for _, cert := range certs {
		if covers(cert.Domain, host) {
			return cert.ID, nil
		}
	}
	return "", nil
}
