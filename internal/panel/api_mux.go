package panel

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// SNI mux group management (admin only; user-owned nodes are rejected in
// validation, and member/portal inbound paths never carry a group).

type muxGroupReq struct {
	NodeID            string `json:"nodeId"`
	Name              string `json:"name"`
	Listen            string `json:"listen"`
	Port              int    `json:"port"`
	FallbackKind      string `json:"fallbackKind"`
	FallbackInboundID string `json:"fallbackInboundId"`
	FallbackTarget    string `json:"fallbackTarget"`
	Enabled           *bool  `json:"enabled"`
}

type muxGroupView struct {
	store.MuxGroup
	MemberCount int64 `json:"memberCount"`
}

func (s *Server) muxGroupView(g store.MuxGroup) muxGroupView {
	return muxGroupView{MuxGroup: g, MemberCount: s.muxMemberCount(g.ID)}
}

func (s *Server) listMuxGroups(c *gin.Context) {
	q := s.st.DB.Order("node_id, port")
	if v := c.Query("node"); v != "" {
		q = q.Where("node_id = ?", v)
	}
	var gs []store.MuxGroup
	if err := q.Find(&gs).Error; err != nil {
		serverError(c, err)
		return
	}
	out := make([]muxGroupView, 0, len(gs))
	for _, g := range gs {
		out = append(out, s.muxGroupView(g))
	}
	c.JSON(http.StatusOK, out)
}

func (r *muxGroupReq) applyTo(g *store.MuxGroup) {
	en := true
	if r.Enabled != nil {
		en = *r.Enabled
	}
	g.Name, g.Listen, g.Port = r.Name, r.Listen, r.Port
	g.FallbackKind, g.FallbackInboundID, g.FallbackTarget = r.FallbackKind, r.FallbackInboundID, r.FallbackTarget
	g.Enabled = en
}

func (s *Server) createMuxGroup(c *gin.Context) {
	var req muxGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	g := store.MuxGroup{ID: store.NewID(), NodeID: req.NodeID}
	req.applyTo(&g)
	if req.Port == 0 {
		g.Port = 443
	}
	if err := s.validateMuxGroup(&g, g.ID); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.st.DB.Create(&g).Error; err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("mux group "+g.Name+" created", g.NodeID)
	s.auditCtx(c, "muxgroup.create", g.Name, fmt.Sprintf("port %d", g.Port))
	c.JSON(http.StatusCreated, s.muxGroupView(g))
}

func (s *Server) updateMuxGroup(c *gin.Context) {
	var g store.MuxGroup
	if err := s.st.DB.First(&g, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req muxGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	members := s.muxMemberCount(g.ID)
	next := g
	req.applyTo(&next)
	next.NodeID = g.NodeID // groups do not move between nodes
	if members > 0 {
		if next.Port != g.Port {
			badRequest(c, fmt.Errorf("复用组已有 %d 个成员，不能改端口：请先移除成员", members))
			return
		}
		if !next.Enabled {
			badRequest(c, fmt.Errorf("复用组已有 %d 个成员，不能停用：请先移除成员", members))
			return
		}
	}
	if err := s.validateMuxGroup(&next, g.ID); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.st.DB.Save(&next).Error; err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("mux group "+next.Name+" updated", next.NodeID)
	s.auditCtx(c, "muxgroup.update", next.Name, fmt.Sprintf("port %d", next.Port))
	c.JSON(http.StatusOK, s.muxGroupView(next))
}

func (s *Server) deleteMuxGroup(c *gin.Context) {
	var g store.MuxGroup
	if err := s.st.DB.First(&g, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if n := s.muxMemberCount(g.ID); n > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("复用组还有 %d 个成员入站，请先把它们移出组", n)})
		return
	}
	if err := s.st.DB.Delete(&g).Error; err != nil {
		serverError(c, err)
		return
	}
	s.MarkDirty("mux group "+g.Name+" deleted", g.NodeID)
	s.auditCtx(c, "muxgroup.delete", g.Name, "")
	c.Status(http.StatusNoContent)
}
