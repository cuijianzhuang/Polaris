package panel

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// cloudflareHTTPSPorts are the HTTPS ports Cloudflare proxies.
var cloudflareHTTPSPorts = []int{443, 2053, 2083, 2087, 2096, 8443}

// createCDNSibling is POST /inbounds/:id/cdn-sibling {cdnProfileId}: for an
// inbound that cannot go through a CDN (TCP, REALITY, UDP protocols), add a
// VLESS + WebSocket + TLS inbound on the same node, on a free port the CDN
// proxies, for the same user groups, published only through the profile.
func (s *Server) createCDNSibling(c *gin.Context) {
	var src store.Inbound
	if err := s.st.DB.First(&src, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req struct {
		CDNProfileID string `json:"cdnProfileId"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", req.CDNProfileID).Error; err != nil {
		badRequest(c, errors.New("请选择 CDN 配置档"))
		return
	}
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", src.NodeID).Error; err != nil {
		dbError(c, err)
		return
	}
	m := model.Inbound{
		NodeID:    node.ID,
		Tag:       s.freeInboundTag(node.ID, src.Tag+"-cdn"),
		Protocol:  model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport: model.TransportSpec{Type: model.TransWS, Path: "/" + randomHex(6), Host: p.OriginDomain},
		Security:  model.SecuritySpec{Type: model.SecTLS, ServerName: p.OriginDomain},
		Sniffing:  src.Sniffing.V,
		Enabled:   true,
	}
	// A port the CDN proxies that nothing on the node uses: the profile's
	// own ports first, then Cloudflare's HTTPS ports.
	candidates := append(slices.Clone(p.Ports.V), cloudflareHTTPSPorts...)
	used, err := s.usedPorts(node.ID)
	if err != nil {
		dbError(c, err)
		return
	}
	for _, port := range candidates {
		if port <= 0 || used[port] {
			continue
		}
		m.Port = port
		if s.portConflict(node.ID, &m, "") == nil {
			break
		}
		m.Port = 0
	}
	if m.Port == 0 {
		badRequest(c, fmt.Errorf("节点 %s 上 %v 这些端口都已被占用，请先在配置档的「可用端口」里加一个空闲端口", node.Name, uniqueInts(candidates)))
		return
	}
	var groups []string
	s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id = ?", src.ID).Pluck("group_id", &groups)
	if groups == nil {
		groups = []string{} // the source had none: keep it that way rather than the defaults
	}
	remark := "CDN · " + orDefault(src.Remark, src.Tag)
	row, err := s.insertInbound(&node, &m, inboundMeta{Remark: remark, OutboundTag: src.OutboundTag, SpeedLimitMbps: src.SpeedLimitMbps}, groups)
	if err != nil {
		badRequest(c, err)
		return
	}
	// Publish it only through the CDN: the direct entry would expose the
	// origin address in subscriptions.
	s.st.DB.Where("inbound_id = ? AND kind = ?", row.ID, "direct").Delete(&store.Endpoint{})
	if err := s.st.DB.Create(&store.Endpoint{ID: store.NewID(), InboundID: row.ID, Kind: "cdn", CDNProfileID: p.ID}).Error; err != nil {
		dbError(c, err)
		return
	}
	addedPort := false
	if !slices.Contains(p.Ports.V, m.Port) {
		p.Ports = store.NewJSON(append(slices.Clone(p.Ports.V), m.Port))
		s.warnWrite("cdn profile ports", s.st.DB.Model(&p).Update("ports", p.Ports).Error)
		addedPort = true
	}
	s.MarkDirty("inbound "+row.Tag+" created for CDN", node.ID)
	s.autoSyncCDNProfile(p.ID)
	s.auditCtx(c, "inbound.cdn_sibling", node.Name+"/"+row.Tag, fmt.Sprintf("from %s via %s :%d", src.Tag, p.Name, m.Port))
	c.JSON(http.StatusCreated, gin.H{"inbound": row, "port": m.Port, "path": m.Transport.Path, "core": m.Core,
		"addedPort": addedPort, "needSync": p.APIToken != "", "profile": p.Name})
}

// freeInboundTag returns base, or base-2, base-3 … when the tag is taken.
func (s *Server) freeInboundTag(nodeID, base string) string {
	if len(base) > 60 {
		base = base[:60]
	}
	tag := base
	for i := 2; ; i++ {
		var n int64
		s.st.DB.Model(&store.Inbound{}).Where("node_id = ? AND tag = ?", nodeID, tag).Count(&n)
		if n == 0 {
			return tag
		}
		tag = fmt.Sprintf("%s-%d", base, i)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func uniqueInts(v []int) []int {
	out := []int{}
	for _, x := range v {
		if x > 0 && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}
