package panel

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Server) tunnelAgentView(p *store.CDNProfile) gin.H {
	var n store.Node
	var c store.Core
	if p.TunnelNodeID != "" {
		s.st.DB.First(&n, "id = ?", p.TunnelNodeID)
		s.st.DB.First(&c, "node_id = ? AND type = ?", n.ID, model.CoreCloudflared)
	}
	cleaned := !p.TunnelManaged && s.tunnelCleanupConfirmed(s.st.DB, p)
	pending := !cleaned
	if p.TunnelManaged {
		pending = n.AppliedRev < n.DesiredRev || s.tunnelAppliedVersion(s.st.DB, p) != p.TunnelCredentialVersion
	}
	var active int64
	s.st.DB.Table("endpoints").Joins("JOIN inbounds ON inbounds.id = endpoints.inbound_id").Where("endpoints.cdn_profile_id = ? AND inbounds.enabled = ?", p.ID, true).Count(&active)
	statusError := c.Error
	if pending && n.LastError != "" {
		statusError = n.LastError
	}
	return gin.H{"credentialMode": p.TunnelCredentialMode, "remoteSupported": slices.Contains(n.AgentFeatures.V, model.RemoteTunnelFeature), "enabled": p.TunnelManaged, "nodeId": p.TunnelNodeID, "hasCredentials": p.TunnelCredential != "", "supported": slices.Contains(n.AgentFeatures.V, model.ManagedTunnelFeature), "nodeStatus": n.Status, "nodeName": n.Name, "pending": pending, "running": c.Status == "running", "error": statusError, "activeEntries": active, "ready": p.TunnelManaged && !pending && c.Status == "running" && c.Error == "" && n.Status == "online", "version": c.RunningVersion, "cleanupConfirmed": cleaned}
}

func (s *Server) tunnelAppliedVersion(db *gorm.DB, p *store.CDNProfile) string {
	var n store.Node
	var r store.ConfigRevision
	if db.First(&n, "id = ?", p.TunnelNodeID).Error != nil || n.AppliedRev == 0 || db.First(&r, "node_id = ? AND rev = ?", n.ID, n.AppliedRev).Error != nil {
		return ""
	}
	raw, err := s.st.Box.Open(r.Spec)
	if err != nil {
		return ""
	}
	var spec model.NodeSpec
	if json.Unmarshal([]byte(raw), &spec) != nil {
		return ""
	}
	for _, t := range spec.Tunnels {
		if t.ProfileID == p.ID {
			return t.CredentialVersion
		}
	}
	return ""
}

func (s *Server) tunnelCleanupConfirmed(db *gorm.DB, p *store.CDNProfile) bool {
	if p.TunnelNodeID == "" {
		return true
	}
	var n store.Node
	if db.First(&n, "id = ?", p.TunnelNodeID).Error != nil || n.AppliedRev == 0 || !slices.Contains(n.AgentFeatures.V, model.ManagedTunnelFeature) {
		return false
	}
	var r store.ConfigRevision
	if db.First(&r, "node_id = ? AND rev = ?", n.ID, n.AppliedRev).Error != nil {
		return false
	}
	raw, err := s.st.Box.Open(r.Spec)
	if err != nil {
		return false
	}
	var spec model.NodeSpec
	if json.Unmarshal([]byte(raw), &spec) != nil {
		return false
	}
	for _, t := range spec.Tunnels {
		if t.ProfileID == p.ID {
			return false
		}
	}
	return true
}
func (s *Server) getTunnelAgent(c *gin.Context) {
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	c.JSON(200, s.tunnelAgentView(&p))
}
func (s *Server) updateTunnelAgent(c *gin.Context) {
	var req struct {
		Enabled        *bool   `json:"enabled"`
		NodeID         string  `json:"nodeId"`
		Credentials    string  `json:"credentials"`
		Token          string  `json:"token"`
		CredentialMode *string `json:"credentialMode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, fmt.Errorf("请提供有效的托管设置"))
		return
	}
	if req.Enabled == nil || len(req.Credentials) > 16384 || len(req.Token) > 16384 {
		badRequest(c, fmt.Errorf("请提供 enabled，凭据大小不得超过 16 KiB"))
		return
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var p store.CDNProfile
	oldNode := ""
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		if err := tx.First(&p, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		oldNode = p.TunnelNodeID
		if !cdnTunnel(&p) {
			return cdnSchemeProblem("请先选择 Cloudflare Tunnel 回源模式")
		}
		if *req.Enabled {
			var node store.Node
			if err := tx.First(&node, "id = ? AND revoked = ?", req.NodeID, false).Error; err != nil {
				return cdnSchemeProblem("请选择有效的 Agent 节点")
			}
			if !slices.Contains(node.AgentFeatures.V, model.ManagedTunnelFeature) {
				return cdnSchemeProblem("请先升级并连接支持 Cloudflare Tunnel 托管的 Agent")
			}
			if oldNode != "" && oldNode != node.ID && (p.TunnelManaged || !s.tunnelCleanupConfirmed(tx, &p)) {
				return cdnSchemeProblem("请先停用托管并等待旧节点清理，再更换节点")
			}
			mode := p.TunnelCredentialMode
			if mode == "" {
				mode = "local"
			}
			oldMode := mode
			if req.CredentialMode != nil {
				mode = *req.CredentialMode
			}
			if mode != "local" && mode != "remote" {
				return cdnSchemeProblem("凭据模式只能是 local 或 remote")
			}
			if mode != oldMode && (p.TunnelManaged || !s.tunnelCleanupConfirmed(tx, &p)) {
				return cdnSchemeProblem("请先停用并等待 Agent 清理，再切换凭据模式")
			}
			if mode == "remote" && !slices.Contains(node.AgentFeatures.V, model.RemoteTunnelFeature) {
				return cdnSchemeProblem("请升级支持远程 Tunnel Token 的 Agent")
			}
			if (mode == "remote" && req.Credentials != "") || (mode == "local" && req.Token != "") {
				return cdnSchemeProblem("请使用所选模式对应的凭据")
			}
			input := req.Credentials
			if mode == "remote" {
				input = req.Token
			}
			if input != "" {
				raw, err := model.NormalizeManagedTunnelCredential([]byte(input), p.TunnelID, mode)
				if err != nil {
					return cdnSchemeProblem(err.Error())
				}
				p.TunnelCredential = s.st.Box.Seal(string(raw))
				p.TunnelCredentialVersion = store.NewID()
			}
			if p.TunnelCredential == "" || mode != oldMode && input == "" {
				return cdnSchemeProblem("请填写所选模式的凭据，远程模式填写 Tunnel Token")
			}
			p.TunnelCredentialMode = mode

			p.TunnelManaged = true
			p.TunnelNodeID = node.ID
			var total int64
			if err := tx.Model(&store.CDNProfile{}).Where("tunnel_managed = ? AND tunnel_node_id = ? AND id <> ?", true, node.ID, p.ID).Count(&total).Error; err != nil {
				return err
			}
			if total >= 32 {
				return cdnSchemeProblem("每台节点最多托管 32 个隧道")
			}
			if err := validateCDNTunnel(tx, &p); err != nil {
				return err
			}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "node_id"}, {Name: "type"}}, DoNothing: true}).Create(&store.Core{ID: store.NewID(), NodeID: node.ID, Type: model.CoreCloudflared, Version: model.CloudflaredVersion, Status: "installing"}).Error; err != nil {
				return err
			}

			if mode == "remote" {
				var connector store.Core
				if err := tx.First(&connector, "node_id = ? AND type = ?", node.ID, model.CoreCloudflared).Error; err != nil {
					return err
				}
				if !core.VersionAtLeast(connector.Version, model.RemoteTunnelMinVersion) {
					return cdnSchemeProblem("远程 Tunnel Token 需要 cloudflared 2025.4.0 或更新版本，请先升级内核")
				}
			}
		} else {
			p.TunnelManaged = false
			p.TunnelCredential = ""
			p.TunnelCredentialVersion = ""
		}
		return tx.Model(&p).Updates(map[string]any{"tunnel_credential_mode": p.TunnelCredentialMode, "tunnel_managed": p.TunnelManaged, "tunnel_node_id": p.TunnelNodeID, "tunnel_credential": p.TunnelCredential, "tunnel_credential_version": p.TunnelCredentialVersion}).Error
	})
	if err != nil {
		if _, ok := err.(cdnSchemeProblem); ok {
			badRequest(c, err)
		} else {
			dbError(c, err)
		}
		return
	}
	nodes := s.nodesForCDNProfile(p.ID)
	if oldNode != "" {
		nodes = append(nodes, oldNode)
	}
	if p.TunnelNodeID != "" {
		nodes = append(nodes, p.TunnelNodeID)
	}
	slices.Sort(nodes)
	s.MarkDirty("cloudflared management changed", slices.Compact(nodes)...)
	s.auditCtx(c, "cdn.tunnel_agent", p.Name, fmt.Sprintf("enabled=%t", p.TunnelManaged))
	c.JSON(200, s.tunnelAgentView(&p))
}

func (s *Server) buildManagedTunnels(spec *model.NodeSpec) error {
	var ps []store.CDNProfile
	if err := s.st.DB.Where("tunnel_managed = ? AND tunnel_node_id = ?", true, spec.NodeID).Order("id").Find(&ps).Error; err != nil {
		return err
	}
	for _, p := range ps {
		t := model.TunnelSpec{ProfileID: p.ID, ID: p.TunnelID, CredentialVersion: p.TunnelCredentialVersion}
		if p.TunnelCredentialMode == "remote" {
			if err := validateCDNTunnel(s.st.DB, &p); err != nil {
				return err
			}
			t.CredentialMode = "remote"
		} else {
			var active int64
			if err := s.st.DB.Table("endpoints").Joins("JOIN inbounds ON inbounds.id = endpoints.inbound_id").Where("endpoints.cdn_profile_id = ? AND inbounds.enabled = ?", p.ID, true).Count(&active).Error; err != nil {
				return err
			}
			if active == 0 {
				continue
			}
			routes, node, err := s.tunnelRoutes(&p)
			if err != nil {
				return err
			}
			if node != spec.NodeID {
				return fmt.Errorf("Tunnel 节点绑定不一致")
			}
			t.Routes = routes
		}
		if err := t.Validate(); err != nil {
			return err
		}
		spec.Tunnels = append(spec.Tunnels, t)
	}
	if len(spec.Tunnels) > 0 && !spec.HasCore(model.CoreCloudflared) {
		spec.Cores = append(spec.Cores, model.CoreSpec{Type: model.CoreCloudflared, Version: model.CloudflaredVersion})
	}
	return nil
}

// Fetch only the currently authorized credential, never an old revision's key.
func (s *Server) deliverTunnelCredentials(nodeID string, tunnels []model.TunnelSpec) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for _, t := range tunnels {
		var p store.CDNProfile
		if err := s.st.DB.First(&p, "id = ? AND tunnel_managed = ? AND tunnel_node_id = ?", t.ProfileID, true, nodeID).Error; err != nil {
			return nil, fmt.Errorf("Tunnel 凭据授权已变化，请同步最新配置")
		}
		if p.TunnelID != t.ID || p.TunnelCredentialVersion != t.CredentialVersion || (p.TunnelCredentialMode == "remote") != (t.CredentialMode == "remote") {
			return nil, fmt.Errorf("Tunnel 凭据已轮换，请同步最新配置")
		}
		if err := validateCDNTunnel(s.st.DB, &p); err != nil {
			return nil, fmt.Errorf("Tunnel 绑定已变化，请同步最新配置")
		}
		raw, err := s.st.Box.Open(p.TunnelCredential)
		if err != nil {
			return nil, fmt.Errorf("Tunnel 凭据解密失败")
		}
		credential, err := model.NormalizeManagedTunnelCredential([]byte(raw), t.ID, t.CredentialMode)
		if err != nil {
			return nil, fmt.Errorf("Tunnel 凭据无效，请重新导入")
		}
		if t.CredentialMode == "remote" {
			out[t.ProfileID], _ = json.Marshal(string(credential))
		} else {
			out[t.ProfileID] = credential
		}
	}
	return out, nil
}
