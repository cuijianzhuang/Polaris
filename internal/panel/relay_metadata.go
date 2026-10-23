package panel

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"unicode"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"golang.org/x/net/idna"
	"gorm.io/gorm"
)

// Pointer fields distinguish omission from clearing an existing override.
// Neither the listening port, path nor tunnel credentials are editable here.
type relayMetadataRequest struct {
	Name         *string `json:"name"`
	ShareName    *string `json:"shareName"`
	Remark       *string `json:"remark"`
	ShareAddress *string `json:"shareAddress"`
	SharePort    *int    `json:"sharePort"`
}

func relayShareAddress(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", nil
	}
	bracketed := strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]")
	if bracketed {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
			return "", fmt.Errorf("分享地址需要可连接的 IP 地址")
		}
		return ip.String(), nil
	}
	if bracketed {
		return "", fmt.Errorf("方括号仅用于 IP 地址")
	}
	ascii, err := idna.Lookup.ToASCII(host)
	ascii = strings.ToLower(strings.TrimSuffix(ascii, "."))
	if err != nil || ascii == "" || len(ascii) > 253 {
		return "", fmt.Errorf("分享地址仅支持 IP 或域名，不带协议、端口和路径")
	}
	for _, label := range strings.Split(ascii, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("分享域名格式无效")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("分享地址仅支持 IP 或域名，不带协议、端口和路径")
			}
		}
	}
	return ascii, nil
}

func (s *Server) updateInboundRelayMetadata(c *gin.Context) {
	var req relayMetadataRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Name == nil && req.ShareName == nil && req.Remark == nil && req.ShareAddress == nil && req.SharePort == nil {
		badRequest(c, fmt.Errorf("请提供要修改的名称、备注或分享地址"))
		return
	}
	if req.Name != nil {
		*req.Name = strings.TrimSpace(*req.Name)
		if *req.Name == "" || len([]rune(*req.Name)) > 64 || strings.IndexFunc(*req.Name, unicode.IsControl) >= 0 {
			badRequest(c, fmt.Errorf("名称需要 1–64 字，不能包含控制字符"))
			return
		}
	}
	if req.Remark != nil && len([]rune(*req.Remark)) > 255 {
		badRequest(c, fmt.Errorf("备注最多 255 字"))
		return
	}
	if req.ShareName != nil {
		*req.ShareName = strings.TrimSpace(*req.ShareName)
		if len([]rune(*req.ShareName)) > 64 || strings.IndexFunc(*req.ShareName, unicode.IsControl) >= 0 {
			badRequest(c, fmt.Errorf("分享名称最多 64 字，不能包含控制字符"))
			return
		}
	}
	if req.ShareAddress != nil {
		host, err := relayShareAddress(*req.ShareAddress)
		if err != nil {
			badRequest(c, err)
			return
		}
		req.ShareAddress = &host
	}
	if req.SharePort != nil && (*req.SharePort < 0 || *req.SharePort > 65535) {
		badRequest(c, fmt.Errorf("分享端口需要 1–65535，0 表示使用线路入口端口"))
		return
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var chain store.Chain
	var inbound store.Inbound
	err := s.st.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var err error
		chain, err = loadInboundRelay(tx, c.Param("rid"), c.Param("id"))
		if err != nil {
			return err
		}
		if err = tx.First(&inbound, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		updates := map[string]any{}
		epUpdates := map[string]any{}
		if req.Name != nil {
			updates["name"] = *req.Name
			epUpdates["share_name"] = *req.Name
			chain.Name = *req.Name
		}
		if req.ShareName != nil {
			epUpdates["share_name"] = *req.ShareName
		}
		if req.Remark != nil {
			updates["remark"] = *req.Remark
			chain.Remark = *req.Remark
		}
		if req.ShareAddress != nil {
			epUpdates["address"] = *req.ShareAddress
		}
		if req.SharePort != nil {
			epUpdates["port"] = *req.SharePort
		}
		// Update all aliases of this managed path without replacing their identity
		// or touching unrelated direct/CDN/manual endpoints.
		endpoints := tx.Model(&store.Endpoint{}).Where("relay_chain_id = ? AND inbound_id = ? AND kind = ?", chain.ID, inbound.ID, "relay")
		var count int64
		if err = endpoints.Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return badInput{fmt.Errorf("中转入口记录缺失，请先修复线路")}
		}
		if len(updates) > 0 {
			if err = tx.Model(&chain).Where("id = ?", chain.ID).Updates(updates).Error; err != nil {
				return err
			}
		}
		if len(epUpdates) > 0 {
			if err = tx.Model(&store.Endpoint{}).Where("relay_chain_id = ? AND inbound_id = ? AND kind = ?", chain.ID, inbound.ID, "relay").Updates(epUpdates).Error; err != nil {
				return err
			}
		}
		audit := store.AuditLog{Admin: c.GetString("admin"), Action: "relay.metadata", Target: chain.ID, Detail: "updated publication metadata", IP: clientIP(c.Request)}
		return tx.Create(&audit).Error
	})
	if err != nil {
		if store.IsDuplicate(err) {
			dbError(c, err)
		} else {
			relaySetupError(c, err)
		}
		return
	}
	// Publication-only changes never dirty node specs or rotate credentials.
	c.JSON(http.StatusOK, s.relayView(chain, inbound))
}
