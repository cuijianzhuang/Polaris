package panel

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

type relayDiagnosticCheck struct {
	Kind      string `json:"kind"`
	NodeID    string `json:"nodeId,omitempty"`
	Target    string `json:"target,omitempty"`
	State     string `json:"state"` // passed / failed / skipped / not_tested
	LatencyMs *int   `json:"latencyMs,omitempty"`
	Message   string `json:"message,omitempty"`
}

type relayDiagnostic struct {
	Kind             string                 `json:"kind"`
	ChainID          string                 `json:"chainId"`
	CheckedAt        time.Time              `json:"checkedAt"`
	State            string                 `json:"state"` // not_ready / failed / partial
	Network          string                 `json:"network"`
	BusinessVerified bool                   `json:"businessVerified"`
	Deployment       inboundRelayView       `json:"deployment"`
	Checks           []relayDiagnosticCheck `json:"checks"`
}

// Diagnose is explicit and read-only. It never records an inbound health
// success, changes routing health, or borrows a billable user's credentials.
func (s *Server) diagnoseInboundRelay(c *gin.Context) {
	ch, err := loadInboundRelay(s.st.DB, c.Param("rid"), c.Param("id"))
	if err != nil {
		dbError(c, err)
		return
	}
	var in store.Inbound
	if err := s.st.DB.First(&in, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	m := s.toModelInbound(&in)
	m.Normalize()
	view := s.relayView(ch, in)
	report := relayDiagnostic{Kind: "network", ChainID: ch.ID, CheckedAt: time.Now(), State: "not_ready", Network: m.Network(), Deployment: view,
		Checks: []relayDiagnosticCheck{{Kind: "deployment", State: "skipped", Message: "配置未在所有节点应用：" + view.State}}}
	if view.State != "applied" {
		report.Checks = append(report.Checks, relayDiagnosticCheck{Kind: "transport", State: "skipped", Message: "线路停用、离线或配置未就绪，未发起网络探测"}, relayBusinessUnchecked())
		c.JSON(http.StatusOK, report)
		return
	}
	nodes, err := relayNodes(s.st.DB, &ch)
	if err != nil {
		badRequest(c, err)
		return
	}
	report.Checks[0] = relayDiagnosticCheck{Kind: "deployment", State: "passed"}
	hops := ch.Hops.V
	checks := make([]relayDiagnosticCheck, len(hops)+1)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m := m // isolate entry-specific address/SNI from the hop checks
		entry := nodes[hops[0].NodeID]
		landing := nodes[in.NodeID]
		m.Port = hops[0].Port
		m.Security.ServerName = orDefault(m.Security.ServerName, landing.Domain)
		check := relayDiagnosticCheck{Kind: "entry_tcp", NodeID: entry.ID, Target: net.JoinHostPort(nodeHost(&entry), strconv.Itoa(m.Port)), State: "skipped"}
		if !m.UsesTCP() {
			check.Message = "UDP 入站不使用 TCP 探测；UDP 业务尚未验证"
			checks[0] = check
			return
		}
		check.Message = "面板到线路入口的 TCP 握手；未验证代理认证或数据转发"
		if m.Security.Type == model.SecTLS || m.Security.Type == model.SecReality {
			check.Kind = "entry_tls_handshake"
			check.Message = "面板经入口检查 TLS 协商；未验证证书、REALITY 或代理认证"
		}
		started := time.Now()
		_, err := probeOne(ctx, nodeHost(&entry), &m)
		check.State = "passed"
		if err != nil {
			check.State, check.Message = "failed", truncate(err.Error(), 250)
		} else {
			ms := int(time.Since(started).Milliseconds())
			check.LatencyMs = &ms
		}
		checks[0] = check
	}()
	for i, h := range hops {
		wg.Add(1)
		go func(i int, h store.ChainHop) {
			defer wg.Done()
			check := relayDiagnosticCheck{Kind: "hop_tcp", NodeID: h.NodeID, State: "skipped"}
			args := map[string]string{}
			if i == len(hops)-1 {
				check.Kind, check.Target = "landing_egress_http", "https://www.gstatic.com/generate_204"
				check.Message = "落地节点自身 HTTP 出口；不经过用户入站或整条中转线路"
				args["http"] = check.Target
			} else {
				if !m.UsesTCP() && ch.TunnelType == "" {
					check.Message = "原样 UDP 转发不使用 TCP 端口探测"
					checks[i+1] = check
					return
				}
				next := hops[i+1]
				n := nodes[next.NodeID]
				port := next.Port
				if ch.TunnelType != "" {
					port = next.TunnelPort
				} else if i+1 == len(hops)-1 {
					port = in.Port
				}
				check.Target = net.JoinHostPort(nodeHost(&n), strconv.Itoa(port))
				check.Message = "此跳到下一跳的 TCP 端口；未验证隧道认证或数据转发"
				args["tcp"] = check.Target
			}
			res, err := s.hub.Command(ctx, h.NodeID, "probe", args, 12*time.Second)
			check.State = "failed"
			switch {
			case err != nil:
				check.Message = truncate(err.Error(), 250)
			case !res.Ok:
				check.Message = truncate(res.Error, 250)
			default:
				ms, err := strconv.Atoi(res.Output)
				if err != nil || ms < 0 {
					check.Message = "节点返回无效的探测结果；请检查 Agent 版本"
				} else {
					check.State, check.LatencyMs = "passed", &ms
				}
			}
			checks[i+1] = check
		}(i, h)
	}
	wg.Wait()
	report.State = "partial" // transport observations never imply proxy success
	for _, check := range checks {
		if check.State == "failed" {
			report.State = "failed"
		}
	}
	report.Checks = append(report.Checks, checks...)
	report.Checks = append(report.Checks, relayBusinessUnchecked())
	c.JSON(http.StatusOK, report)
}

func relayBusinessUnchecked() relayDiagnosticCheck {
	return relayDiagnosticCheck{Kind: "proxy_business", State: "not_tested", Message: "网络诊断未执行代理认证、上下行或 UDP 端到端测试；请运行支持的真实业务诊断，或使用真实订阅验证业务"}
}
