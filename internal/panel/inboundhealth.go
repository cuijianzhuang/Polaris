package panel

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Inbound health: every few minutes the panel connects to each inbound the
// way a client would reach it (node address and port; a TLS handshake for
// TLS and REALITY, with REALITY's server name), so a closed port, a cloud
// firewall or a broken REALITY target shows up before users report it.
// UDP-only inbounds (Hysteria 2, TUIC, KCP, QUIC) cannot be probed this way.
// Three failures in a row alert; the recovery alerts too.

const (
	probeTimeout   = 5 * time.Second
	probeAlertAt   = 3
	probeWorkers   = 16
	probeUDPReason = "UDP 入站无法从面板探测"
)

type probeTarget struct {
	in   store.Inbound
	node *store.Node
	m    model.Inbound
}

// probeOne connects to host:port, then shakes hands for TLS / REALITY.
func probeOne(ctx context.Context, host string, m *model.Inbound) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	t0 := time.Now()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(m.Port)))
	if err != nil {
		return 0, simplifyNetErr(err)
	}
	defer conn.Close()
	lat := time.Since(t0)
	sni := ""
	switch m.Security.Type {
	case model.SecTLS:
		sni = orDefault(m.Security.ServerName, host)
	case model.SecReality:
		sni = first(m.Security.RealityServerNames)
	default:
		return lat, nil
	}
	tc := tls.Client(conn, &tls.Config{ServerName: sni, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // reachability only
	if err := tc.HandshakeContext(ctx); err != nil {
		return lat, fmt.Errorf("TLS 握手失败：%w", simplifyNetErr(err))
	}
	return lat, nil
}

func simplifyNetErr(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("连接被拒绝（端口未监听或被防火墙拒绝）")
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "deadline exceeded"):
		return fmt.Errorf("连接超时（安全组 / 防火墙未放行，或网络不通）")
	case strings.Contains(msg, "no such host"):
		return fmt.Errorf("域名无法解析")
	case strings.Contains(msg, "connection reset"):
		return fmt.Errorf("连接被重置")
	}
	return err
}

// probeTargets lists the inbounds to check: enabled, public, TCP, on an
// online node with an address.
func (s *Server) probeTargets(only string) []probeTarget {
	q := s.st.DB.Where("enabled = ?", true)
	if only != "" {
		q = q.Where("id = ?", only)
	}
	var ins []store.Inbound
	q.Find(&ins)
	nodes := map[string]*store.Node{}
	var ns []store.Node
	s.st.DB.Where("revoked = ?", false).Find(&ns)
	for i := range ns {
		nodes[ns[i].ID] = &ns[i]
	}
	var out []probeTarget
	for _, in := range ins {
		if in.RelayOnly {
			continue // direct reachability is deliberately not published
		}
		n := nodes[in.NodeID]
		if n == nil || nodeHost(n) == "" || isLoopbackHost(in.Listen) {
			continue
		}
		if only == "" && n.Status != "online" && n.Status != "syncing" {
			continue // the node-offline alert already covers it
		}
		out = append(out, probeTarget{in: in, node: n, m: s.toModelInbound(&in)})
	}
	return out
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || strings.HasPrefix(h, "127.") || h == "::1"
}

// probeInbounds checks every target and records the outcome.
func (s *Server) probeInbounds() {
	if s.st.GetSetting("inbound_probe", "true") != "true" {
		return
	}
	s.runProbes(s.probeTargets(""))
}

func (s *Server) runProbes(ts []probeTarget) {
	sem := make(chan struct{}, probeWorkers)
	var wg sync.WaitGroup
	for _, t := range ts {
		wg.Add(1)
		sem <- struct{}{}
		go func(t probeTarget) {
			defer func() { <-sem; wg.Done() }()
			s.probeAndRecord(t)
		}(t)
	}
	wg.Wait()
}

func (s *Server) probeAndRecord(t probeTarget) store.InboundHealth {
	now := time.Now()
	var h store.InboundHealth
	if s.st.DB.First(&h, "inbound_id = ?", t.in.ID).Error != nil {
		h = store.InboundHealth{InboundID: t.in.ID, OK: true, Since: now}
	}
	if !t.m.UsesTCP() {
		h = store.InboundHealth{InboundID: t.in.ID, OK: true, Error: probeUDPReason, Since: h.Since, CheckedAt: now}
		s.st.DB.Save(&h)
		return h
	}
	lat, err := probeOne(s.bg, nodeHost(t.node), &t.m)
	wasOK := h.OK
	h.CheckedAt = now
	label := fmt.Sprintf("%s · %s（%s:%d）", t.node.Name, t.in.Tag, nodeHost(t.node), t.m.Port)
	if err != nil {
		h.OK, h.Error, h.LatencyMs = false, truncate(err.Error(), 250), 0
		h.Fails++
		if h.Fails >= probeAlertAt && !h.Alerted {
			h.Alerted = true
			s.notify(Event{Kind: "inbound_down", NodeID: t.node.ID, Text: fmt.Sprintf("🔴 入站无法连接：%s\n%s（连续 %d 次）", label, h.Error, h.Fails)})
		}
	} else {
		if h.Alerted {
			s.notify(Event{Kind: "inbound_up", NodeID: t.node.ID, Text: fmt.Sprintf("🟢 入站已恢复：%s，延迟 %d ms", label, lat.Milliseconds())})
		}
		h.OK, h.Error, h.Fails, h.Alerted, h.LatencyMs = true, "", 0, false, int(lat.Milliseconds())
	}
	if h.OK != wasOK || h.Since.IsZero() {
		h.Since = now
	}
	s.st.DB.Save(&h)
	return h
}

// probeInboundAPI: POST /inbounds/:id/probe — check one inbound now.
func (s *Server) probeInboundAPI(c *gin.Context) {
	ts := s.probeTargets(c.Param("id"))
	if len(ts) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无法直连探测：入站未启用、仅本机监听、仅通过中转提供，或节点没有地址；中转线路请使用线路诊断"})
		return
	}
	c.JSON(http.StatusOK, s.probeAndRecord(ts[0]))
}
