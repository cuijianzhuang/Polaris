package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

func (s *Server) diagnoseInboundRelayBusiness(c *gin.Context) {
	var targets struct {
		TCPURL     string `json:"tcpUrl"`
		UDPAddress string `json:"udpAddress"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&targets); err != nil && err != io.EOF {
			badRequest(c, err)
			return
		}
	}
	ch, err := loadInboundRelay(s.st.DB, c.Param("rid"), c.Param("id"))
	if err != nil {
		dbError(c, err)
		return
	}
	var in store.Inbound
	if err = s.st.DB.First(&in, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	view := s.relayView(ch, in)
	if view.State != "applied" {
		c.JSON(http.StatusOK, gin.H{"state": "not_ready", "businessVerified": false, "deployment": view})
		return
	}
	var landing store.Node
	if err = s.st.DB.First(&landing, "id = ?", in.NodeID).Error; err != nil {
		dbError(c, err)
		return
	}
	if in.Core != model.CoreXray || in.Protocol.V.Type != model.ProtoVLESS || !contains(landing.AgentFeatures.V, model.RelayBusinessFeature) {
		c.JSON(http.StatusOK, gin.H{"state": "unsupported", "businessVerified": false, "message": "当前支持升级后的 Linux Agent 与 Xray VLESS；请先升级落地 Agent"})
		return
	}
	req := model.RelayBusinessRequest{ID: store.NewID() + store.NewID(), ExpiresAt: time.Now().Add(15 * time.Second).Unix(), InboundTag: in.Tag, ServerName: orDefault(in.Security.V.ServerName, landing.Domain), EntryAddress: view.Address, EntryPort: view.Port, TCPURL: orDefault(targets.TCPURL, "https://www.gstatic.com/generate_204"), UDPAddress: orDefault(targets.UDPAddress, "1.1.1.1:53")}
	if view.ShareAddress != "" {
		req.EntryAddress = view.ShareAddress
	}
	if view.SharePort > 0 {
		req.EntryPort = view.SharePort
	}
	raw, _ := json.Marshal(req)
	s.auditCtx(c, "relay.diagnose.start", ch.ID, req.ID)
	outcome := "interrupted"
	defer func() { s.auditCtx(c, "relay.diagnose.finish", ch.ID, req.ID+" "+outcome) }()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 23*time.Second)
	defer cancel()
	res, err := s.hub.Command(ctx, in.NodeID, model.RelayBusinessFeature, map[string]string{"request": string(raw)}, 22*time.Second)
	if err != nil {
		cancelCtx, cancelDone := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.hub.Command(cancelCtx, in.NodeID, model.RelayBusinessFeature+"/cancel", map[string]string{"id": req.ID}, 4*time.Second)
		cancelDone()
		c.JSON(http.StatusOK, gin.H{"state": "failed", "businessVerified": false, "message": "诊断中断或节点未返回结果，不能确认业务或清理成功"})
		return
	}
	var result model.RelayBusinessResult
	if json.Unmarshal([]byte(res.Output), &result) != nil {
		c.JSON(http.StatusOK, gin.H{"state": "failed", "businessVerified": false, "message": "节点返回无效诊断结果"})
		return
	}
	// Do not trust a positive flag alone. Require both real data checks and
	// acknowledged credential cleanup, even from a buggy/stale agent.
	tcp, udp, cleanup := false, false, false
	checksOK := true
	for _, check := range result.Checks {
		if check.State != "passed" {
			checksOK = false
		}
		if check.Kind == "credential_cleanup" && check.State == "passed" {
			cleanup = true
		}
		if check.Kind == "proxy_tcp_business" && check.State == "passed" {
			tcp = true
		}
		if check.Kind == "proxy_udp_business" && check.State == "passed" {
			udp = true
		}
	}
	result.BusinessVerified = res.Ok && result.State == "passed" && result.CleanupVerified && tcp && udp && cleanup && checksOK
	if !result.BusinessVerified && result.State == "passed" {
		result.State = "failed"
	}
	if !res.Ok && res.Error != "" {
		result.Checks = append(result.Checks, model.BusinessCheck{Kind: "execution", State: "failed", Message: fmt.Sprintf("节点诊断未完成：%.250s", res.Error)})
	}
	result.CheckedAt = time.Now().UTC()
	outcome = result.State
	c.JSON(http.StatusOK, result)
}
