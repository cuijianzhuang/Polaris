package panel

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"github.com/gin-gonic/gin"
)

type portalEntryCheck struct {
	State     string    `json:"state"`
	Stage     string    `json:"stage"`
	Message   string    `json:"message"`
	LatencyMs int64     `json:"latencyMs"`
	CheckedAt time.Time `json:"checkedAt"`
}

func portalNodeKey(n sub.Node) string { return portalLinkKey(n.Name, sub.ShareLink(n)) }

// This socket check never authenticates, installs temporary users, restarts a
// kernel, or charges proxy traffic. Its evidence belongs to this portal user.
func checkPortalEntry(ctx context.Context, n sub.Node, dial func(context.Context, string, string) (net.Conn, error)) portalEntryCheck {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := portalEntryCheck{State: "unsupported", Stage: "tcp", CheckedAt: time.Now().UTC()}
	m := model.Inbound{Protocol: model.ProtocolSpec{Type: n.Protocol}, Transport: n.Transport}
	if !m.UsesTCP() {
		out.Message = "此协议使用 UDP；TCP 连接检查不适用，请在客户端测试，业务验证需管理员使用对应适配。"
		return out
	}
	start := time.Now()
	conn, err := dial(ctx, "tcp", net.JoinHostPort(strings.Trim(n.Server, "[]"), strconv.Itoa(n.Port)))
	if err == nil {
		defer conn.Close()
		if n.TLS.Type == model.SecTLS {
			out.Stage = "tls"
			name := n.TLS.ServerName
			if name == "" {
				name = strings.Trim(n.Server, "[]")
			}
			tc := tls.Client(conn, &tls.Config{ServerName: name, InsecureSkipVerify: n.TLS.Insecure, MinVersion: tls.VersionTLS12, NextProtos: n.TLS.ALPN}) //nolint:gosec // follows the published client TLS policy
			err = tc.HandshakeContext(ctx)
		}
	}
	out.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		out.State = "failed"
		out.Message = "入口连接失败，请稍后重试或联系支持。"
		var dns *net.DNSError
		switch {
		case errors.Is(err, errPrivateTarget):
			out.State = "unsupported"
			out.Message = "此入口解析到内部网络，用户中心不执行内部地址检查，请联系管理员。"
		case errors.Is(err, context.DeadlineExceeded):
			out.Message = "入口连接超时；请检查监听端口、安全组及防火墙。"
		case errors.As(err, &dns):
			out.Message = "入口域名解析失败，请联系管理员检查 DNS。"
		default:
			if e, ok := err.(net.Error); ok && e.Timeout() {
				out.Message = "入口连接超时；请检查监听端口、安全组及防火墙。"
			} else if out.Stage == "tls" {
				out.Message = "TCP 已连通，TLS 握手或证书校验失败，请联系管理员检查 SNI 与证书。"
			}
		}
		return out
	}
	out.State = "passed"
	out.Message = "面板到订阅入口的 TCP 连接通过；尚未验证代理认证、数据传输及 UDP，本地网络情况请在客户端测试。"
	if out.Stage == "tls" {
		out.Message = "面板到订阅入口的 TCP 与 TLS 握手通过；尚未验证代理认证、数据传输及 UDP，本地网络情况请在客户端测试。"
	}
	if n.TLS.Type == model.SecReality {
		out.Message = "入口 TCP 连接通过；尚未验证 REALITY 公钥、Short ID、代理认证及数据传输，请在客户端测试。"
	}
	return out
}

func portalEntryDial(ctx context.Context, network, address string) (net.Conn, error) {
	d := net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		return checkSourceIP(net.ParseIP(host), false)
	}}
	return d.DialContext(ctx, network, address)
}

func portalCheckCacheKey(user, key string) string { return "portal-entry-check:" + user + ":" + key }

func (s *Server) applyPortalEntryChecks(info *sub.Info, userID string) {
	for i := range info.Links {
		link := &info.Links[i]
		var check portalEntryCheck
		if !s.eph.get(portalCheckCacheKey(userID, link.Key), &check) || check.State == "unsupported" {
			continue
		}
		old, _ := time.Parse(time.RFC3339, link.CheckedAt)
		if !check.CheckedAt.After(old) {
			continue
		}
		link.Network, link.CheckScope, link.CheckedAt = check.State, "面板 → 订阅入口", check.CheckedAt.Format(time.RFC3339)
		// Keep deployment/offline failures, and never turn a socket check into
		// business proof. A newer failed connection overrides an older success.
		if check.State == "failed" {
			link.State, link.StateLabel, link.Reason = "unavailable", "暂不可用", check.Message
		} else if link.State == "unknown" {
			link.StateLabel, link.Reason = "连接通过", check.Message
		}
	}
}

func (s *Server) portalNodeCheck(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req struct {
		Key string `json:"key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if !s.servedUser(u) {
		c.JSON(http.StatusForbidden, gin.H{"error": "账号当前没有可用服务"})
		return
	}
	nodes, err := s.subNodesFor(u, s.subCarrier(c.Request, u), s.subRegion(c.Request, u))
	if err != nil {
		serverError(c, err)
		return
	}
	var target *sub.Node
	for i := range nodes {
		if nodes[i].Source != "info" && sub.ShareLink(nodes[i]) != "" && portalNodeKey(nodes[i]) == req.Key {
			target = &nodes[i]
			break
		}
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "节点已撤销或不在当前订阅中，请刷新列表"})
		return
	}
	if !s.shared.allow("portal-entry-user", 6, u.ID) || !s.shared.allow("portal-entry-global", 60, "all") || !s.shared.once("portal-entry-target:"+portalLinkKey(target.Server, strconv.Itoa(target.Port)), 15*time.Second) {
		c.Header("Retry-After", "15")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "检查较频繁，请等待 15 秒后重试"})
		return
	}
	result := checkPortalEntry(c.Request.Context(), *target, portalEntryDial)
	if err := s.eph.put(portalCheckCacheKey(u.ID, req.Key), result, 5*time.Minute); err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
