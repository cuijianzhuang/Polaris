package panel

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// SNI mux groups ("443 端口复用"): several TCP + TLS / REALITY inbounds
// share one public port. The agent's built-in splitter reads the
// ClientHello SNI and passes the connection, still encrypted, to the
// member's loopback port; each member keeps its own certificate and SNI.
// Members are told apart from the two older port-sharing mechanisms by
// mutual exclusion (design D10): a member cannot be Caddy-fronted and
// cannot be a fallback child.

// muxEligible reports whether an inbound may join an SNI mux group: a
// TCP transport whose TLS the core itself terminates (TLS or REALITY).
// HTTP-shaped transports stay with Caddy fronting instead.
func muxEligible(m *model.Inbound) bool {
	mm := *m
	mm.Normalize()
	if mm.Transport.Type != model.TransTCP || !mm.UsesTCP() {
		return false
	}
	return mm.Security.Type == model.SecTLS || mm.Security.Type == model.SecReality
}

// muxMemberSNIs derives the SNIs a member answers to from its own TLS
// configuration — they are never stored separately (design D2).
func muxMemberSNIs(m *model.Inbound) []string {
	switch m.Security.Type {
	case model.SecTLS:
		if s := strings.ToLower(strings.TrimSpace(m.Security.ServerName)); s != "" {
			return []string{s}
		}
	case model.SecReality:
		var out []string
		for _, s := range m.Security.RealityServerNames {
			if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// certCoversSNI reports whether the stored certificate's SANs cover sni.
func (s *Server) certCoversSNI(certID, sni string) error {
	var c store.Certificate
	if err := s.st.DB.First(&c, "id = ?", certID).Error; err != nil {
		return fmt.Errorf("成员证书不存在")
	}
	block, _ := pem.Decode([]byte(c.Cert))
	if block == nil {
		return fmt.Errorf("成员证书无法解析")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("成员证书无法解析：%w", err)
	}
	if err := cert.VerifyHostname(sni); err != nil {
		return fmt.Errorf("SNI %s 不在成员证书（%s）的覆盖范围内", sni, c.Domain)
	}
	return nil
}

// validateMuxMember checks an inbound that joins (or stays in) a mux
// group. Called from validateInbound after the certificate is resolved.
func (s *Server) validateMuxMember(node *store.Node, m *model.Inbound, selfID string) error {
	if m.MuxGroupID == "" {
		return nil
	}
	if node.OwnerID != "" {
		return fmt.Errorf("用户自带节点暂不支持 SNI 复用组")
	}
	var g store.MuxGroup
	if err := s.st.DB.First(&g, "id = ?", m.MuxGroupID).Error; err != nil {
		return fmt.Errorf("SNI 复用组不存在")
	}
	if g.NodeID != node.ID {
		return fmt.Errorf("SNI 复用组「%s」不在同一节点", g.Name)
	}
	if !g.Enabled {
		return fmt.Errorf("SNI 复用组「%s」已停用，不能加入成员", g.Name)
	}
	if m.Port != g.Port {
		return fmt.Errorf("复用组成员的端口必须与组端口 %d 一致（当前 %d）", g.Port, m.Port)
	}
	if !muxEligible(m) {
		return fmt.Errorf("只有 TCP + TLS / REALITY 入站可以加入 SNI 复用组（WS / gRPC 等 HTTP 形传输请继续使用 Caddy 前置）")
	}
	if selfID != "" {
		var row store.Inbound
		if err := s.st.DB.First(&row, "id = ?", selfID).Error; err == nil {
			if row.RelayAccess.V.Enabled {
				return fmt.Errorf("已启用中转来源限制的入站不能加入 SNI 复用组")
			}
		}
		if _, fronted := frontProfile(s.st.DB, selfID); fronted {
			return fmt.Errorf("入站已通过 Caddy 前置发布，不能同时加入 SNI 复用组")
		}
		if master := s.referencingMaster(selfID); master != "" {
			return fmt.Errorf("入站是「%s」的回落子入站，不能同时加入 SNI 复用组", master)
		}
	}
	snis := muxMemberSNIs(m)
	if len(snis) == 0 {
		return fmt.Errorf("成员入站缺少 SNI（TLS 需填写服务器名称，REALITY 需填写 serverNames）")
	}
	if m.Security.Type == model.SecTLS {
		for _, sni := range snis {
			if err := s.certCoversSNI(m.Security.CertificateID, sni); err != nil {
				return err
			}
		}
	}
	if !m.Enabled {
		return nil // a disabled member is not routed; SNI clashes surface when it is enabled
	}
	var members []store.Inbound
	s.st.DB.Where("node_id = ? AND mux_group_id = ? AND id <> ? AND enabled = ?", node.ID, g.ID, selfID, true).Find(&members)
	taken := map[string]string{}
	for i := range members {
		om := s.toModelInbound(&members[i])
		for _, sni := range muxMemberSNIs(&om) {
			taken[sni] = members[i].Tag
		}
	}
	for _, sni := range snis {
		if tag, dup := taken[sni]; dup {
			return fmt.Errorf("SNI %s 已被同组成员「%s」占用", sni, tag)
		}
	}
	return nil
}

// validateMuxGroup checks a group being created or updated; selfID is
// the group's own id (its members carry it and are exempt from the port
// occupation check). Member-dependent rules live here rather than in
// the inbound path because the group owns the public port.
func (s *Server) validateMuxGroup(g *store.MuxGroup, selfID string) error {
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", g.NodeID).Error; err != nil {
		return fmt.Errorf("节点不存在")
	}
	if node.OwnerID != "" {
		return fmt.Errorf("用户自带节点暂不支持 SNI 复用组")
	}
	if strings.TrimSpace(g.Name) == "" {
		return fmt.Errorf("复用组名称不能为空")
	}
	if g.Port <= 0 || g.Port > 65535 {
		return fmt.Errorf("端口 %d 超出范围", g.Port)
	}
	var dup int64
	s.st.DB.Model(&store.MuxGroup{}).Where("node_id = ? AND port = ? AND id <> ?", g.NodeID, g.Port, selfID).Count(&dup)
	if dup > 0 {
		return fmt.Errorf("该节点上已有复用组占用端口 %d（同节点同端口至多一组）", g.Port)
	}
	// The group's port must be free of everything that is not its member:
	// standalone inbounds, Caddy fronts, websites, forwards, chain hops.
	var ins []store.Inbound
	s.st.DB.Where("node_id = ?", g.NodeID).Find(&ins)
	for i := range ins {
		in := &ins[i]
		if in.Port == g.Port && in.MuxGroupID != selfID {
			om := s.toModelInbound(in)
			if in.Enabled && om.UsesTCP() {
				return fmt.Errorf("端口 %d 已被入站「%s」占用：先把它加入本组，或换一个组端口", g.Port, in.Tag)
			}
		}
		if fi, fronted := frontProfile(s.st.DB, in.ID); fronted && fi.Port == g.Port {
			return fmt.Errorf("端口 %d 已被 Caddy 前置（入站「%s」）占用", g.Port, in.Tag)
		}
	}
	var sites []store.Website
	s.st.DB.Where("node_id = ? AND port = ? AND enabled = ?", g.NodeID, g.Port, true).Find(&sites)
	if len(sites) > 0 {
		return fmt.Errorf("端口 %d 已被网站「%s」占用", g.Port, sites[0].Domain)
	}
	var fwds []store.ForwardRule
	s.st.DB.Where("node_id = ? AND listen_port = ?", g.NodeID, g.Port).Find(&fwds)
	if len(fwds) > 0 {
		return fmt.Errorf("端口 %d 已被转发规则「%s」占用", g.Port, fwds[0].Name)
	}
	var chains []store.Chain
	s.st.DB.Where("mode = ?", "l4").Find(&chains)
	for _, c := range chains {
		for _, h := range c.Hops.V {
			if h.NodeID == g.NodeID && (h.Port == g.Port || h.TunnelPort == g.Port) {
				return fmt.Errorf("端口 %d 已被中转线路「%s」占用", g.Port, c.Name)
			}
		}
	}
	switch g.FallbackKind {
	case model.MuxFallbackNone:
	case model.MuxFallbackMember:
		if g.FallbackInboundID == "" {
			return fmt.Errorf("请选择兜底成员入站")
		}
		var fb store.Inbound
		if err := s.st.DB.First(&fb, "id = ?", g.FallbackInboundID).Error; err != nil {
			return fmt.Errorf("兜底成员入站不存在")
		}
		if fb.NodeID != g.NodeID {
			return fmt.Errorf("兜底成员入站不在同一节点")
		}
		if fb.MuxGroupID != "" && fb.MuxGroupID != selfID {
			return fmt.Errorf("兜底入站「%s」属于另一个复用组", fb.Tag)
		}
	case model.MuxFallbackExternal:
		host, port, err := net.SplitHostPort(strings.TrimSpace(g.FallbackTarget))
		if err != nil || host == "" {
			return fmt.Errorf("回落目标需为 host:port 形式")
		}
		if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
			return fmt.Errorf("回落目标端口无效")
		}
	default:
		return fmt.Errorf("未知回落方式 %q", g.FallbackKind)
	}
	return nil
}

// muxMemberCount counts a group's member inbounds.
func (s *Server) muxMemberCount(groupID string) int64 {
	var n int64
	s.st.DB.Model(&store.Inbound{}).Where("mux_group_id = ?", groupID).Count(&n)
	return n
}

// buildMuxes rewrites member inbounds in the spec to loopback ports and
// returns the agent's mux dispatch table. internalBase continues the
// fronting allocation so the two never collide. Groups with no routed
// member emit nothing; a member whose group is missing or disabled is
// left untouched (it binds its port itself) and logged — validation
// keeps that state unreachable in practice.
func (s *Server) buildMuxes(spec *model.NodeSpec, internalBase int) []model.MuxSpec {
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", spec.NodeID).Error; err != nil {
		return nil
	}
	if node.OwnerID != "" {
		// User-owned nodes never get mux groups (design D8); a stray
		// membership is ignored rather than dispatched.
		return nil
	}
	var groups []store.MuxGroup
	s.st.DB.Where("node_id = ? AND enabled = ?", spec.NodeID, true).Order("port, name").Find(&groups)
	if len(groups) == 0 {
		return nil
	}
	byGroup := map[string]*store.MuxGroup{}
	for i := range groups {
		byGroup[groups[i].ID] = &groups[i]
	}
	n := 0
	var out []model.MuxSpec
	for _, g := range groups {
		ms := model.MuxSpec{ID: g.ID, Name: g.Name, Listen: g.Listen, Port: g.Port}
		seen := map[string]bool{}
		internalOf := map[string]string{}
		tagOf := map[string]string{}
		for i := range spec.Inbounds {
			in := &spec.Inbounds[i]
			if in.MuxGroupID != g.ID || !in.Enabled {
				continue
			}
			if !muxEligible(in) {
				s.log.Warn("mux member skipped: not TCP+TLS/REALITY", "group", g.Name, "inbound", in.Tag)
				continue
			}
			snis := muxMemberSNIs(in)
			if len(snis) == 0 {
				s.log.Warn("mux member skipped: no SNI", "group", g.Name, "inbound", in.Tag)
				continue
			}
			internal := internalBase + n
			n++
			upstream := "127.0.0.1:" + strconv.Itoa(internal)
			internalOf[in.ID] = upstream
			tagOf[in.ID] = in.Tag
			for _, sni := range snis {
				if seen[sni] {
					s.log.Warn("mux SNI claimed twice, first member wins", "group", g.Name, "sni", sni)
					continue
				}
				seen[sni] = true
				ms.Routes = append(ms.Routes, model.MuxRoute{SNI: sni, Upstream: upstream, Inbound: in.Tag})
			}
			in.Listen, in.Port = "127.0.0.1", internal
		}
		if len(ms.Routes) == 0 {
			continue
		}
		switch g.FallbackKind {
		case model.MuxFallbackMember:
			if up, ok := internalOf[g.FallbackInboundID]; ok {
				ms.Fallback = up
				ms.FallbackInbound = tagOf[g.FallbackInboundID]
			} else {
				s.log.Warn("mux fallback member is not a routed member; unmatched traffic drops", "group", g.Name)
			}
		case model.MuxFallbackExternal:
			ms.Fallback = g.FallbackTarget
		}
		out = append(out, ms)
	}
	for i := range spec.Inbounds {
		in := &spec.Inbounds[i]
		if in.MuxGroupID != "" {
			if _, ok := byGroup[in.MuxGroupID]; !ok {
				s.log.Warn("mux member's group is missing or disabled; inbound keeps its port", "inbound", in.Tag)
			}
		}
	}
	return out
}
