package panel

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// exportInbound is GET /inbounds/:id/export: the inbound in the create
// request format, secrets included, so it can be imported elsewhere and
// existing clients keep working.
func (s *Server) exportInbound(c *gin.Context) {
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	m := s.toModelInbound(&row)
	var groups []string
	s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id = ?", row.ID).Pluck("group_id", &groups)
	enabled, relayOnly := row.Enabled, row.RelayOnly
	req := inboundReq{NodeID: row.NodeID, Core: m.Core, Tag: m.Tag, Listen: m.Listen, Port: m.Port, Protocol: m.Protocol,
		Transport: m.Transport, Security: m.Security, Sniffing: m.Sniffing, Extra: m.Extra, Enabled: &enabled, RelayOnly: &relayOnly,
		Remark: row.Remark, GroupIDs: groups, OutboundTag: row.OutboundTag, SpeedLimitMbps: row.SpeedLimitMbps}
	req.Security.CertPEM, req.Security.KeyPEM = "", ""
	s.auditCtx(c, "inbound.export", row.Tag, "")
	c.Header("Content-Disposition", "attachment; filename=inbound-"+row.Tag+".json")
	c.JSON(http.StatusOK, req)
}

// cloneInbound is POST /inbounds/:id/clone {nodeIds, port}: copies the inbound
// (settings, keys, groups) to each node. On the same node the copy gets the
// next free port and a "-copy" tag; elsewhere it keeps them when free.
func (s *Server) cloneInbound(c *gin.Context) {
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req struct {
		NodeIDs []string `json:"nodeIds"`
		Port    int      `json:"port"`
	}
	_ = c.ShouldBindJSON(&req)
	if len(req.NodeIDs) == 0 {
		req.NodeIDs = []string{row.NodeID}
	}
	var groups []string
	s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id = ?", row.ID).Pluck("group_id", &groups)
	if groups == nil {
		groups = []string{}
	}
	type result struct {
		Node  string `json:"node"`
		Tag   string `json:"tag,omitempty"`
		Port  int    `json:"port,omitempty"`
		Error string `json:"error,omitempty"`
	}
	var out []result
	for _, nid := range req.NodeIDs {
		var node store.Node
		if err := s.st.DB.First(&node, "id = ?", nid).Error; err != nil {
			out = append(out, result{Node: nid, Error: "节点不存在"})
			continue
		}
		m := s.toModelInbound(&row)
		m.ID, m.NodeID = "", node.ID
		if node.ID == row.NodeID && row.Core != "" {
			m.Core = row.Core
		} else {
			m.Core = model.CoreAuto // let the target node pick a core that supports it
		}
		m.Tag = s.freeTag(node.ID, row.Tag, node.ID == row.NodeID)
		if req.Port > 0 {
			m.Port = req.Port
		}
		// Find a free port: the original one on another node when possible.
		for i := 0; i < 50; i++ {
			if s.portConflict(node.ID, &m, "") == nil {
				break
			}
			m.Port++
		}
		r, err := s.insertInbound(&node, &m, inboundMeta{Remark: row.Remark, OutboundTag: row.OutboundTag, SpeedLimitMbps: row.SpeedLimitMbps, RelayOnly: row.RelayOnly}, groups)
		if err != nil {
			out = append(out, result{Node: node.Name, Error: err.Error()})
			continue
		}
		s.MarkDirty("inbound "+r.Tag+" cloned", node.ID)
		out = append(out, result{Node: node.Name, Tag: r.Tag, Port: r.Port})
	}
	s.auditCtx(c, "inbound.clone", row.Tag, fmt.Sprintf("to %d nodes", len(req.NodeIDs)))
	c.JSON(http.StatusOK, gin.H{"results": out})
}

// freePort is GET /nodes/:id/free-port?min=&max=: a random port no inbound,
// chain or other program on the node uses (default 20000–60000).
func (s *Server) freePort(c *gin.Context) {
	lo, _ := strconv.Atoi(c.Query("min"))
	hi, _ := strconv.Atoi(c.Query("max"))
	p := s.randomFreePort(c.Param("id"), lo, hi)
	if p == 0 {
		badRequest(c, fmt.Errorf("没有可用端口"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"port": p})
}

// freeTag returns tag, or tag-copy / tag-copy2 … when it is taken on the node.
func (s *Server) freeTag(nodeID, tag string, forceCopy bool) string {
	var tags []string
	s.st.DB.Model(&store.Inbound{}).Where("node_id = ?", nodeID).Pluck("tag", &tags)
	taken := map[string]bool{}
	for _, t := range tags {
		taken[t] = true
	}
	if !forceCopy && !taken[tag] {
		return tag
	}
	base := strings.TrimSuffix(tag, "-copy")
	for i := 1; ; i++ {
		t := base + "-copy"
		if i > 1 {
			t += fmt.Sprint(i)
		}
		if !taken[t] {
			return t
		}
	}
}
