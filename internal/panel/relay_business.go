package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

type relayBusinessSupport struct {
	Supported       bool   `json:"supported"`
	Reason          string `json:"reason,omitempty"`
	Scope           string `json:"scope,omitempty"`
	RequiresRestart bool   `json:"requiresRestart,omitempty"`
}

func relayBusinessAvailability(in store.Inbound, landing store.Node, installed ...store.Core) relayBusinessSupport {
	if in.Core == model.CoreSingbox {
		m := model.Inbound{Core: in.Core, Protocol: in.Protocol.V, Transport: in.Transport.V, Security: in.Security.V, Extra: in.Extra}
		if !model.SingboxRelayBusinessInbound(m) {
			return relayBusinessSupport{Reason: "sing-box 真实业务诊断仅支持 VLESS + TCP + REALITY/Vision，且不能使用入站 JSON 覆盖或共享回落；此限制不影响当前协议的中转创建和转发"}
		}
		if !contains(landing.AgentFeatures.V, model.SingboxRelayBusinessFeature) {
			return relayBusinessSupport{Reason: "落地 Agent 尚未声明 sing-box 安全临时授权能力，请更新或同步 Agent 后重试"}
		}
		for _, cr := range installed {
			if cr.NodeID != in.NodeID || cr.Type != in.Core {
				continue
			}
			v := cr.RunningVersion
			if v == "" || v == "latest" || strings.Contains(v, "-") || !core.VersionAtLeast(v, model.SingboxRelayBusinessMinVersion) {
				return relayBusinessSupport{Reason: "sing-box 真实业务诊断要求已上报的稳定内核版本不低于 1.13.5"}
			}
			if cr.NoUserTraffic {
				return relayBusinessSupport{Reason: "sing-box 真实业务诊断要求 with_v2ray_api 构建，以独立计量诊断流量"}
			}
			return relayBusinessSupport{Supported: true, RequiresRestart: true, Scope: "sing-box VLESS + TCP + REALITY/Vision；诊断开始和结束时重启内核，现有连接会中断"}
		}
		return relayBusinessSupport{Reason: "sing-box 尚未上报运行版本与计量能力，请等待 Agent 心跳"}
	}
	if in.Core != model.CoreXray || in.Protocol.V.Type != model.ProtoVLESS {
		return relayBusinessSupport{Reason: "真实业务诊断支持 Xray VLESS 和 sing-box VLESS TCP REALITY/Vision；此限制不影响当前协议的中转创建和转发"}
	}
	if !contains(landing.AgentFeatures.V, model.RelayBusinessFeature) {
		return relayBusinessSupport{Reason: "落地 Agent 尚未声明真实业务诊断能力，请更新或同步 Agent 后重试"}
	}
	return relayBusinessSupport{Supported: true}
}

func (s *Server) relayBusinessSupport(in store.Inbound, landing store.Node) relayBusinessSupport {
	var installed []store.Core
	if in.Core == model.CoreSingbox {
		if err := s.st.DB.Where("node_id = ? AND type = ?", in.NodeID, in.Core).Find(&installed).Error; err != nil {
			return relayBusinessSupport{Reason: "无法读取 sing-box 运行版本与计量能力"}
		}
	}
	return relayBusinessAvailability(in, landing, installed...)
}

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
		c.JSON(http.StatusOK, gin.H{"kind": "business", "state": "not_ready", "businessVerified": false, "deployment": view})
		return
	}
	var landing store.Node
	if err = s.st.DB.First(&landing, "id = ?", in.NodeID).Error; err != nil {
		dbError(c, err)
		return
	}
	if support := s.relayBusinessSupport(in, landing); !support.Supported {
		c.JSON(http.StatusOK, gin.H{"kind": "business", "state": "unsupported", "businessVerified": false, "message": support.Reason})
		return
	}
	req := model.RelayBusinessRequest{Core: in.Core, ID: store.NewID() + store.NewID(), ExpiresAt: time.Now().Add(15 * time.Second).Unix(), InboundTag: in.Tag, ServerName: orDefault(in.Security.V.ServerName, landing.Domain), EntryAddress: view.Address, EntryPort: view.Port, TCPURL: orDefault(targets.TCPURL, relayBusinessTCP), UDPAddress: orDefault(targets.UDPAddress, relayBusinessUDP)}
	if view.ShareAddress != "" {
		req.EntryAddress = view.ShareAddress
	}
	if view.SharePort > 0 {
		req.EntryPort = view.SharePort
	}
	raw, _ := json.Marshal(req)
	standard := req.TCPURL == relayBusinessTCP && req.UDPAddress == relayBusinessUDP
	if standard {
		if err := s.beginRelayBusinessStatus(ch, in, req); err != nil {
			serverError(c, err)
			return
		}
	}
	s.auditCtx(c, "relay.diagnose.start", ch.ID, req.ID)
	outcome := "interrupted"
	var result model.RelayBusinessResult
	defer func() {
		if standard {
			if err := s.finishRelayBusinessStatus(ch.ID, req.ID, outcome, result); err != nil {
				s.log.Warn("persist relay business status", "chain", ch.ID, "error", err)
			}
		}
	}()
	defer func() { s.auditCtx(c, "relay.diagnose.finish", ch.ID, req.ID+" "+outcome) }()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 23*time.Second)
	defer cancel()
	res, err := s.hub.Command(ctx, in.NodeID, model.RelayBusinessFeature, map[string]string{"request": string(raw)}, 22*time.Second)
	if err != nil {
		cancelCtx, cancelDone := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.hub.Command(cancelCtx, in.NodeID, model.RelayBusinessFeature+"/cancel", map[string]string{"id": req.ID}, 4*time.Second)
		cancelDone()
		c.JSON(http.StatusOK, gin.H{"kind": "business", "state": "failed", "businessVerified": false, "message": "诊断中断或节点未返回结果，不能确认业务或清理成功"})
		return
	}
	if json.Unmarshal([]byte(res.Output), &result) != nil {
		outcome = "failed"
		c.JSON(http.StatusOK, gin.H{"kind": "business", "state": "failed", "businessVerified": false, "message": "节点返回无效诊断结果"})
		return
	}
	// Do not trust a positive flag alone. Require both real data checks and
	// acknowledged credential cleanup, even from a buggy/stale agent.
	tcp, udp, cleanup, authentication, transfer := false, false, false, false, false
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
		if check.Kind == "proxy_authentication" && check.State == "passed" {
			authentication = true
		}
		if check.Kind == "proxy_transfer" && check.State == "passed" && check.UpBytes > 0 && check.DownBytes > 0 {
			transfer = true
		}
	}
	if in.Core == model.CoreSingbox {
		checksOK = checksOK && authentication && transfer
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
	c.JSON(http.StatusOK, struct {
		Kind string `json:"kind"`
		model.RelayBusinessResult
	}{"business", result})
}
