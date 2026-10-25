package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type sourceReference struct {
	ID         string   `json:"id"`
	NodeID     string   `json:"nodeId"`
	Tag        string   `json:"tag"`
	InboundIDs []string `json:"inboundIds"`
}
type sourceNodeView struct {
	store.SubSourceNode
	Source     string            `json:"source"`
	Available  bool              `json:"available"`
	Supported  bool              `json:"supported"`
	Reason     string            `json:"reason,omitempty"`
	References []sourceReference `json:"references"`
}

func (s *Server) sourceView(src *store.SubSource) {
	src.HasURL, src.HasContent = src.URL != "", src.Content != ""
	if plain, e := s.st.Box.Open(src.URL); e == nil {
		if u, e := url.Parse(plain); e == nil {
			src.URLHost = u.Hostname()
		}
	}
}
func sourceReferences(db *gorm.DB, id string) ([]sourceReference, error) {
	var obs []store.Outbound
	if e := db.Where("source_node_id = ?", id).Order("id").Find(&obs).Error; e != nil {
		return nil, e
	}
	out := []sourceReference{}
	for _, o := range obs {
		refs := []string{}
		if e := db.Model(&store.Inbound{}).Where("node_id = ? AND outbound_tag = ?", o.NodeID, o.Tag).Order("id").Pluck("id", &refs).Error; e != nil {
			return nil, e
		}
		out = append(out, sourceReference{ID: o.ID, NodeID: o.NodeID, Tag: o.Tag, InboundIDs: refs})
	}
	return out, nil
}
func (s *Server) openSourceNode(row store.SubSourceNode) (sub.Node, error) {
	var n sub.Node
	plain, e := s.st.Box.Open(row.Payload)
	if e != nil {
		return n, fmt.Errorf("节点凭据无法解密")
	}
	if e = json.Unmarshal([]byte(plain), &n); e != nil {
		return n, fmt.Errorf("节点凭据记录无效")
	}
	return n, nil
}
func sourceOutbound(n sub.Node) (model.Outbound, error) {
	o := model.Outbound{}
	t, ok := outboundTypeFor(n.Protocol)
	if !ok {
		return o, fmt.Errorf("该协议暂不支持服务器出站引用")
	}
	if n.Server == "" || n.Port < 1 || n.Port > 65535 || strings.ContainsAny(n.Server, "\r\n/ ") {
		return o, fmt.Errorf("节点地址或端口无效")
	}
	if n.Obfs != "" && n.Protocol != model.ProtoHysteria2 {
		return o, fmt.Errorf("暂不支持该混淆方式的服务器出站引用")
	}
	if n.Protocol == model.ProtoHysteria2 && n.Obfs != "" && n.Obfs != "salamander" {
		return o, fmt.Errorf("暂不支持该 Hysteria2 混淆方式")
	}
	if n.VisionUDP443 || n.PQRealityOnly {
		return o, fmt.Errorf("该节点的扩展选项尚未支持服务器出站引用")
	}
	set := model.OutboundSettings{Server: strings.Trim(n.Server, "[]"), Port: n.Port, Username: n.Username, Password: n.Password, UUID: n.UUID, Method: n.Method, Flow: n.Flow, Encryption: n.Encryption, UpMbps: n.UpMbps, DownMbps: n.DownMbps, ObfsPassword: n.ObfsPassword}
	if n.Transport.Type != "" && n.Transport.Type != model.TransTCP && n.Transport.Type != model.TransQUIC {
		tr := n.Transport
		set.Transport = &tr
	}
	if n.TLS.Type != "" && n.TLS.Type != model.SecNone {
		tls := n.TLS
		set.Security = &tls
	}
	b, e := json.Marshal(set)
	if e != nil {
		return o, e
	}
	o.Type, o.Settings = t, b
	return o, nil
}
func (s *Server) sourceNodeForReference(db *gorm.DB, id string) (store.SubSourceNode, sub.Node, error) {
	var row store.SubSourceNode
	var src store.SubSource
	if e := db.First(&row, "id = ?", id).Error; e != nil {
		return row, sub.Node{}, fmt.Errorf("订阅节点不存在")
	}
	if e := db.First(&src, "id = ?", row.SourceID).Error; e != nil {
		return row, sub.Node{}, fmt.Errorf("订阅源不存在")
	}
	if row.Missing || !src.Enabled || src.Cache == "" {
		return row, sub.Node{}, fmt.Errorf("订阅节点已移除、来源已停用或等待解析")
	}
	n, e := s.openSourceNode(row)
	return row, n, e
}
func (s *Server) sourceOutboundOn(db *gorm.DB, nodeID, id string) (model.Outbound, error) {
	_, n, e := s.sourceNodeForReference(db, id)
	if e != nil {
		return model.Outbound{}, e
	}
	o, e := sourceOutbound(n)
	if e != nil {
		return o, e
	}
	if e = s.nodeSupportsOutboundOn(db, nodeID, o.Type); e != nil {
		return o, e
	}
	var cores []store.Core
	if e = db.Where("node_id = ?", nodeID).Find(&cores).Error; e != nil {
		return o, e
	}
	for _, cr := range cores {
		if cr.Type != model.CoreXray && cr.Type != model.CoreSingbox && cr.Type != model.CoreMihomo {
			continue
		}
		caps, _ := core.Capabilities(cr.Type, cr.Version)
		if cr.Type == model.CoreXray && n.TLS.Type == model.SecTLS && n.TLS.Insecure && core.VersionAtLeast(cr.Version, "26.3.27") {
			return o, fmt.Errorf("该 Xray 版本不再支持跳过 TLS 证书校验，请使用有效证书的来源节点")
		}

		tr, sec := n.Transport.Type, n.TLS.Type
		if tr == "" {
			tr = model.TransTCP
		}
		if sec == "" {
			sec = model.SecNone
		}
		if !caps.Supports(n.Protocol, tr, sec) {
			return o, fmt.Errorf("%s 不支持该节点的协议、传输与安全组合", cr.Type)
		}
		if n.Encryption != "" && n.Encryption != "none" {
			if cr.Type != model.CoreXray {
				return o, fmt.Errorf("该节点的 VLESS Encryption 暂仅支持 Xray 出站")
			}
			has := false
			for _, f := range caps.Features {
				has = has || f == model.FeatureVLESSEncryption
			}
			if !has {
				return o, fmt.Errorf("请升级 Xray 后使用该节点的 VLESS Encryption")
			}
		}
	}
	return o, nil
}

// Missing/disabled/unsupported resources keep their tag as a block outbound.
// Never keep using removed credentials or fall back to the core's direct path.
func (s *Server) resolveSourceRef(o store.Outbound) model.Outbound {
	resolved, e := s.sourceOutboundOn(s.st.DB, o.NodeID, o.SourceNodeID)
	if e != nil {
		resolved = model.Outbound{Type: model.OutBlock}
	}
	resolved.ID, resolved.NodeID, resolved.Tag, resolved.Detour = o.ID, o.NodeID, o.Tag, o.Detour
	if resolved.Type == model.OutBlock {
		resolved.Detour = ""
	}
	return resolved
}
func (s *Server) sourceResourceView(row store.SubSourceNode) (sourceNodeView, error) {
	v := sourceNodeView{SubSourceNode: row, References: []sourceReference{}}
	var src store.SubSource
	if e := s.st.DB.First(&src, "id = ?", row.SourceID).Error; e != nil {
		return v, e
	}
	v.Source = src.Name
	_, n, e := s.sourceNodeForReference(s.st.DB, row.ID)
	v.Available = e == nil
	if e != nil {
		v.Reason = e.Error()
	} else if _, e = sourceOutbound(n); e != nil {
		v.Reason = e.Error()
	} else {
		v.Supported = true
	}
	v.References, e = sourceReferences(s.st.DB, row.ID)
	return v, e
}
func (s *Server) sourceResourcesAPI(c *gin.Context) {
	q := s.st.DB.Order("source_id, missing, position, id")
	if c.Param("id") != "" {
		q = q.Where("source_id = ?", c.Param("id"))
	}
	var rows []store.SubSourceNode
	if e := q.Find(&rows).Error; e != nil {
		dbError(c, e)
		return
	}
	out := []sourceNodeView{}
	for _, row := range rows {
		v, e := s.sourceResourceView(row)
		if e != nil {
			dbError(c, e)
			return
		}
		out = append(out, v)
	}
	c.JSON(http.StatusOK, out)
}
func (s *Server) sourceRefreshesAPI(c *gin.Context) {
	var rows []store.SubSourceRefresh
	if e := s.st.DB.Where("source_id = ?", c.Param("id")).Order("revision DESC").Limit(100).Find(&rows).Error; e != nil {
		dbError(c, e)
		return
	}
	if rows == nil {
		rows = []store.SubSourceRefresh{}
	}
	c.JSON(http.StatusOK, rows)
}
func (s *Server) deleteSourceResource(c *gin.Context) {
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := store.ReserveSourceResources(tx); e != nil {
			return e
		}
		id := c.Param("id")
		var src store.SubSource
		if e := tx.First(&src, "id = ?", id).Error; e != nil {
			return e
		}
		var count int64
		if e := tx.Model(&store.Outbound{}).Where("source_node_id IN (?)", tx.Model(&store.SubSourceNode{}).Select("id").Where("source_id = ?", id)).Count(&count).Error; e != nil {
			return e
		}
		if count > 0 {
			return fmt.Errorf("仍有 %d 个出站或中转引用，请先解除引用", count)
		}
		var profiles []store.SubProfile
		if e := tx.Find(&profiles).Error; e != nil {
			return e
		}
		for _, p := range profiles {
			for _, sid := range p.SourceIDs.V {
				if sid == id {
					return fmt.Errorf("仍被订阅组 %s 引用，请先解除引用", p.Name)
				}
			}
		}
		if e := tx.Delete(&store.SubSourceNode{}, "source_id = ?", id).Error; e != nil {
			return e
		}
		if e := tx.Delete(&store.SubSourceRefresh{}, "source_id = ?", id).Error; e != nil {
			return e
		}
		return tx.Delete(&src).Error
	})
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	s.auditCtx(c, "sub_source.delete", c.Param("id"), "")
	c.Status(http.StatusNoContent)
}
func (s *Server) deleteSourceNodeAPI(c *gin.Context) {
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := store.ReserveSourceResources(tx); e != nil {
			return e
		}
		var row store.SubSourceNode
		if e := tx.First(&row, "id = ?", c.Param("nid")).Error; e != nil {
			return e
		}
		refs, e := sourceReferences(tx, row.ID)
		if e != nil {
			return e
		}
		if len(refs) > 0 {
			return fmt.Errorf("节点仍被出站或中转引用，请先解除引用")
		}
		if !row.Missing {
			return fmt.Errorf("只能清理已从来源移除的节点")
		}
		return tx.Delete(&row).Error
	})
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	s.auditCtx(c, "sub_source_node.delete", c.Param("nid"), "")
	c.Status(http.StatusNoContent)
}

// The optional inbound binding implements protocol relay through an existing
// authorized local inbound. Users/quotas still belong to the local inbound;
// supplier credentials are never copied into share links or API responses.
func (s *Server) bindSourceNodeAPI(c *gin.Context) {
	if !s.requireFeature(c, license.FeatureSubConvert) {
		return
	}
	var req struct {
		NodeID    string `json:"nodeId" binding:"required"`
		Tag       string `json:"tag" binding:"required"`
		InboundID string `json:"inboundId"`
	}
	if e := c.ShouldBindJSON(&req); e != nil {
		badRequest(c, e)
		return
	}
	if !inScope(c, req.NodeID) {
		c.Status(http.StatusForbidden)
		return
	}
	o := store.Outbound{ID: store.NewID()}
	err := s.mutateOutbounds(req.NodeID, func(tx *gorm.DB) error {
		if e := s.storeOutboundTx(tx, req.NodeID, &o, &outboundReq{Tag: req.Tag, Type: model.OutSourceRef, SourceNodeID: c.Param("nid")}); e != nil {
			return e
		}
		if req.InboundID != "" {
			var in store.Inbound
			if e := tx.First(&in, "id = ? AND node_id = ?", req.InboundID, req.NodeID).Error; e != nil {
				return e
			}
			if in.OutboundTag != "" {
				return fmt.Errorf("入站已有出站绑定，请先在入站中解除绑定")
			}
			if in.Core == model.CoreHysteria {
				return fmt.Errorf("请在 Xray、sing-box 或 mihomo 入站上配置协议中转")
			}
			var n int64
			if e := tx.Model(&store.RoutingRule{}).Where("node_id = ? AND chain_id <> ''", req.NodeID).Count(&n).Error; e != nil {
				return e
			}
			if n > 0 {
				return fmt.Errorf("该节点已有链路路由，请在路由配置中显式绑定出站")
			}
			if e := tx.Model(&in).Update("outbound_tag", o.Tag).Error; e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		badRequest(c, err)
		return
	}
	s.MarkDirty("subscription node reference", req.NodeID)
	s.auditCtx(c, "sub_source_node.bind", c.Param("nid"), req.NodeID+":"+req.Tag)
	c.JSON(http.StatusCreated, o)
}

// A reject alias cannot be a native outbound on recent sing-box. Resolve its
// dependent graph to the built-in block action before sending any core a spec.
// Conservatively suspend groups/detours using a missing resource as well.
func collapseBlockedSourcePaths(spec *model.NodeSpec) {
	blocked := map[string]bool{}
	for _, o := range spec.Outbounds {
		if o.Type == model.OutBlock {
			blocked[o.Tag] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, o := range spec.Outbounds {
			if blocked[o.Tag] {
				continue
			}
			deps, e := o.OutboundDependencies()
			if e != nil {
				continue
			}
			for _, dep := range deps {
				if blocked[dep] {
					blocked[o.Tag] = true
					changed = true
					break
				}
			}
		}
	}
	if len(blocked) == 0 {
		return
	}
	keep := spec.Outbounds[:0]
	for _, o := range spec.Outbounds {
		if !blocked[o.Tag] || o.Tag == "block" {
			keep = append(keep, o)
		}
	}
	spec.Outbounds = keep
	for i := range spec.Routes {
		if blocked[spec.Routes[i].Outbound] {
			spec.Routes[i].Outbound = "block"
		}
	}
}
