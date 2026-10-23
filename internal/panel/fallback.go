package panel

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Fallbacks: one VLESS / Trojan TCP + TLS / REALITY inbound (the master)
// answers on the public port and hands handshakes it does not accept to
// child inbounds listening on loopback, matched by path / ALPN. Children are
// ordinary inbounds; their share links advertise the master's port and TLS.

func isLoopback(listen string) bool {
	if listen == "localhost" {
		return true
	}
	ip := net.ParseIP(listen)
	return ip != nil && ip.IsLoopback()
}

// validateFallbacks checks a master's fallback list against its children.
func (s *Server) validateFallbacks(m *model.Inbound, selfID string) error {
	fbs := m.Protocol.Fallbacks
	if len(fbs) == 0 {
		return nil
	}
	if !m.FallbackCapable() {
		return fmt.Errorf("回落仅支持 VLESS / Trojan + TCP + TLS / REALITY 的入站")
	}
	if m.Core != model.CoreXray {
		return fmt.Errorf("回落需要 Xray 内核（当前为 %s）", m.Core)
	}
	seen := map[string]bool{}
	defaults := 0
	for i := range fbs {
		f := &fbs[i]
		if f.InboundID == "" || f.InboundID == selfID {
			return fmt.Errorf("回落 #%d：请选择另一个入站", i+1)
		}
		if f.Path != "" && !strings.HasPrefix(f.Path, "/") {
			return fmt.Errorf("回落 #%d：路径需以 / 开头", i+1)
		}
		if f.ALPN != "" && f.ALPN != "h2" && f.ALPN != "http/1.1" {
			return fmt.Errorf("回落 #%d：ALPN 只能是 h2 或 http/1.1", i+1)
		}
		key := f.Path + "|" + f.ALPN
		if seen[key] {
			return fmt.Errorf("回落 #%d：与前面的规则匹配条件重复", i+1)
		}
		seen[key] = true
		if f.Path == "" && f.ALPN == "" {
			defaults++
		}
		var child store.Inbound
		if err := s.st.DB.First(&child, "id = ?", f.InboundID).Error; err != nil {
			return fmt.Errorf("回落 #%d：子入站不存在", i+1)
		}
		if child.RelayAccess.V.Enabled {
			return fmt.Errorf("回落子入站已启用中转来源限制，请使用独立入站")
		}
		if child.NodeID != m.NodeID {
			return fmt.Errorf("回落 #%d：子入站「%s」不在同一节点", i+1, child.Tag)
		}
		if child.Security.V.Type != "" && child.Security.V.Type != model.SecNone {
			return fmt.Errorf("回落 #%d：子入站「%s」不能再开 TLS / REALITY（TLS 由主入站处理）", i+1, child.Tag)
		}
		if !isLoopback(child.Listen) {
			return fmt.Errorf("回落 #%d：子入站「%s」需监听 127.0.0.1，否则会绕过主入站直接暴露在公网", i+1, child.Tag)
		}
		f.Dest = "" // resolved when the spec is built
	}
	if defaults > 1 {
		return fmt.Errorf("最多只能有一条不带路径 / ALPN 的默认回落")
	}
	return nil
}

// resolveFallbacks fills Dest for every fallback of the node's inbounds.
func resolveFallbacks(ins []model.Inbound) {
	byID := map[string]*model.Inbound{}
	for i := range ins {
		byID[ins[i].ID] = &ins[i]
	}
	for i := range ins {
		for j := range ins[i].Protocol.Fallbacks {
			f := &ins[i].Protocol.Fallbacks[j]
			f.Dest = ""
			if c, ok := byID[f.InboundID]; ok && c.Enabled {
				f.Dest = net.JoinHostPort(orDefault(c.Listen, "127.0.0.1"), strconv.Itoa(c.Port))
			}
		}
	}
}

// fallbackMasters maps child inbound id -> its master, for the given nodes.
func (s *Server) fallbackMasters(nodeIDs []string) map[string]*store.Inbound {
	out := map[string]*store.Inbound{}
	if len(nodeIDs) == 0 {
		return out
	}
	var ins []store.Inbound
	s.st.DB.Where("node_id IN ? AND enabled = ?", nodeIDs, true).Find(&ins)
	for i := range ins {
		for _, f := range ins[i].Protocol.V.Fallbacks {
			out[f.InboundID] = &ins[i]
		}
	}
	return out
}

// referencingMaster returns the tag of a master that falls back to id.
func (s *Server) referencingMaster(id string) string {
	var row store.Inbound
	if s.st.DB.First(&row, "id = ?", id).Error != nil {
		return ""
	}
	if m := s.fallbackMasters([]string{row.NodeID})[id]; m != nil {
		return m.Tag
	}
	return ""
}
