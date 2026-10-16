package panel

import (
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

// Standalone port forwards (realm): a forwarding panel inside Polaris, after
// realm-xwPF — several remotes with load balancing and failover, ws / tls /
// wss tunnels between a relay and a landing realm, PROXY protocol.

type forwardView struct {
	store.ForwardRule
	NodeName string `json:"nodeName"`
}

func (s *Server) listForwards(c *gin.Context) {
	var rs []store.ForwardRule
	q := s.st.DB.Order("node_id, listen_port")
	if n := c.Query("node"); n != "" {
		q = q.Where("node_id = ?", n)
	}
	q.Find(&rs)
	names := s.nodeNames()
	out := make([]forwardView, 0, len(rs))
	for _, r := range rs {
		out = append(out, forwardView{r, names[r.NodeID]})
	}
	c.JSON(http.StatusOK, out)
}

type forwardReq struct {
	NodeID      string                `json:"nodeId" binding:"required"`
	Name        string                `json:"name"`
	ListenIP    string                `json:"listenIp"`
	ListenPort  int                   `json:"listenPort"`
	Network     string                `json:"network"`
	Remotes     []store.ForwardRemote `json:"remotes"`
	Balance     string                `json:"balance"`
	Failover    bool                  `json:"failover"`
	Through     string                `json:"through"`
	TunnelRole  string                `json:"tunnelRole"`
	TunnelType  string                `json:"tunnelType"`
	TunnelHost  string                `json:"tunnelHost"`
	TunnelPath  string                `json:"tunnelPath"`
	TunnelSNI   string                `json:"tunnelSni"`
	SendProxy   int                   `json:"sendProxy"`
	AcceptProxy bool                  `json:"acceptProxy"`
	Enabled     *bool                 `json:"enabled"`
	Remark      string                `json:"remark"`
}

func (r *forwardReq) validate() error {
	if r.ListenPort < 1 || r.ListenPort > 65535 {
		return fmt.Errorf("监听端口无效")
	}
	switch r.Network {
	case "", "tcp,udp":
		r.Network = "tcp,udp"
	case "tcp", "udp":
	default:
		return fmt.Errorf("协议应为 tcp / udp / tcp,udp")
	}
	if r.ListenIP != "" && net.ParseIP(r.ListenIP) == nil {
		return fmt.Errorf("监听地址应为 IP")
	}
	if r.Through != "" && net.ParseIP(r.Through) == nil {
		return fmt.Errorf("出口地址应为 IP")
	}
	n := 0
	for i := range r.Remotes {
		a := strings.TrimSpace(r.Remotes[i].Addr)
		h, p, err := net.SplitHostPort(a)
		if err != nil || h == "" {
			return fmt.Errorf("目标 %q 应为 host:port", a)
		}
		if pn, err := strconv.Atoi(p); err != nil || pn < 1 || pn > 65535 {
			return fmt.Errorf("目标 %q 端口无效", a)
		}
		r.Remotes[i].Addr = a
		if r.Remotes[i].Weight < 0 {
			r.Remotes[i].Weight = 0
		}
		if r.Remotes[i].Enabled {
			n++
		}
	}
	if n == 0 {
		return fmt.Errorf("至少需要一个启用的目标")
	}
	switch r.Balance {
	case "", "roundrobin", "iphash":
	default:
		return fmt.Errorf("负载均衡应为 roundrobin / iphash")
	}
	switch r.TunnelRole {
	case "":
		r.TunnelType = ""
	case "client", "server":
		if r.Network == "udp" {
			return fmt.Errorf("加密隧道只支持 TCP")
		}
		switch r.TunnelType {
		case "ws", "tls", "wss":
		default:
			return fmt.Errorf("隧道类型应为 ws / tls / wss")
		}
	default:
		return fmt.Errorf("隧道角色应为 client / server")
	}
	if r.SendProxy < 0 || r.SendProxy > 2 {
		return fmt.Errorf("PROXY protocol 版本应为 1 或 2")
	}
	return nil
}

func (r *forwardReq) apply(row *store.ForwardRule) {
	row.NodeID, row.Name, row.ListenIP, row.ListenPort, row.Network = r.NodeID, strings.TrimSpace(r.Name), r.ListenIP, r.ListenPort, r.Network
	row.Remotes, row.Balance, row.Failover, row.Through = store.NewJSON(r.Remotes), r.Balance, r.Failover, r.Through
	row.TunnelRole, row.TunnelType, row.TunnelHost, row.TunnelPath, row.TunnelSNI = r.TunnelRole, r.TunnelType, r.TunnelHost, r.TunnelPath, r.TunnelSNI
	row.SendProxy, row.AcceptProxy, row.Remark = r.SendProxy, r.AcceptProxy, r.Remark
	if r.Enabled != nil {
		row.Enabled = *r.Enabled
	}
	if row.Name == "" {
		row.Name = fmt.Sprintf("fwd-%d", r.ListenPort)
	}
}

// forwardPortConflict checks a forward's listen port against inbounds, l4
// chains and other forwards on the node.
func (s *Server) forwardPortConflict(r *forwardReq, excludeID string) error {
	m := model.Inbound{Port: r.ListenPort}
	// A stand-in inbound with the same networks for portConflict.
	switch r.Network {
	case "udp":
		m.Protocol.Type, m.Transport.Type = model.ProtoHysteria2, model.TransQUIC
	case "tcp":
		m.Protocol.Type, m.Transport.Type = model.ProtoVLESS, model.TransTCP
	default:
		m.Protocol.Type, m.Transport.Type = model.ProtoShadowsocks, model.TransTCP
	}
	m.Normalize()
	if err := s.portConflict(r.NodeID, &m, excludeID); err != nil {
		return err
	}
	var others []store.ForwardRule
	s.st.DB.Where("node_id = ? AND listen_port = ? AND id <> ?", r.NodeID, r.ListenPort, excludeID).Find(&others)
	for _, o := range others {
		if netOverlap(o.Network, r.Network) {
			return fmt.Errorf("端口 %d 已被转发规则 %s 使用", r.ListenPort, o.Name)
		}
	}
	return nil
}

func netOverlap(a, b string) bool {
	for _, x := range strings.Split(a, ",") {
		if strings.Contains(b, x) {
			return true
		}
	}
	return false
}

func (s *Server) createForward(c *gin.Context) {
	var req forwardReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.ListenPort == 0 {
		req.ListenPort = s.randomFreePort(req.NodeID, 0, 0)
	}
	if err := req.validate(); err != nil {
		badRequest(c, err)
		return
	}
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", req.NodeID).Error; err != nil {
		dbError(c, err)
		return
	}
	if err := s.forwardPortConflict(&req, ""); err != nil {
		badRequest(c, err)
		return
	}
	row := store.ForwardRule{ID: store.NewID(), Enabled: true, Health: store.NewJSON(map[string]bool{})}
	req.apply(&row)
	if err := s.st.DB.Create(&row).Error; err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("forward "+row.Name+" created", row.NodeID)
	s.auditCtx(c, "forward.create", node.Name+"/"+row.Name, fmt.Sprintf(":%d → %d targets", row.ListenPort, len(req.Remotes)))
	c.JSON(http.StatusCreated, row)
}

func (s *Server) updateForward(c *gin.Context) {
	var row store.ForwardRule
	if err := s.st.DB.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req forwardReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := req.validate(); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.forwardPortConflict(&req, row.ID); err != nil {
		badRequest(c, err)
		return
	}
	oldNode := row.NodeID
	req.apply(&row)
	row.Health = store.NewJSON(map[string]bool{})
	if err := s.st.DB.Save(&row).Error; err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("forward "+row.Name+" updated", oldNode, row.NodeID)
	s.auditCtx(c, "forward.update", row.Name, "")
	c.JSON(http.StatusOK, row)
}

func (s *Server) deleteForward(c *gin.Context) {
	var row store.ForwardRule
	if err := s.st.DB.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	s.st.DB.Delete(&row)
	s.MarkDirty("forward "+row.Name+" deleted", row.NodeID)
	s.auditCtx(c, "forward.delete", row.Name, "")
	c.Status(http.StatusNoContent)
}

// tunnelString builds a realm transport string for a rule's tunnel side.
func tunnelString(r *store.ForwardRule) string {
	host := r.TunnelHost
	path := r.TunnelPath
	if path == "" {
		path = "/"
	}
	sni := r.TunnelSNI
	if sni == "" {
		sni = host
	}
	ws := "ws;host=" + host + ";path=" + path
	if r.TunnelRole == "client" {
		tls := "tls;sni=" + sni + ";insecure"
		switch r.TunnelType {
		case "ws":
			return ws
		case "tls":
			return tls
		case "wss":
			return ws + ";" + tls
		}
	}
	tls := "tls;servername=" + sni // realm signs a certificate for it
	switch r.TunnelType {
	case "ws":
		return ws
	case "tls":
		return tls
	case "wss":
		return ws + ";" + tls
	}
	return ""
}

// standaloneForwards renders a node's enabled forward rules, leaving out
// remotes failover marked down (all of them down: keep them all).
func (s *Server) standaloneForwards(nodeID string) []model.ForwardRule {
	var rs []store.ForwardRule
	s.st.DB.Where("node_id = ? AND enabled = ?", nodeID, true).Order("listen_port").Find(&rs)
	var out []model.ForwardRule
	for i := range rs {
		r := &rs[i]
		var targets []string
		var weights []int
		var all []store.ForwardRemote
		for _, rm := range r.Remotes.V {
			if rm.Enabled {
				all = append(all, rm)
			}
		}
		for _, rm := range all {
			if r.Failover && r.Health.V[rm.Addr] {
				continue
			}
			targets = append(targets, rm.Addr)
			weights = append(weights, max(rm.Weight, 1))
		}
		if len(targets) == 0 {
			for _, rm := range all {
				targets = append(targets, rm.Addr)
				weights = append(weights, max(rm.Weight, 1))
			}
		}
		if len(targets) == 0 {
			continue
		}
		f := model.ForwardRule{Tag: "fwd-" + r.ID, ListenIP: r.ListenIP, Listen: r.ListenPort, Network: r.Network,
			Target: targets[0], ExtraTargets: targets[1:], Balance: r.Balance, Weights: weights, Through: r.Through,
			SendProxy: r.SendProxy, AcceptProxy: r.AcceptProxy}
		switch r.TunnelRole {
		case "client":
			f.RemoteTransport = tunnelString(r)
		case "server":
			f.ListenTransport = tunnelString(r)
		}
		out = append(out, f)
	}
	return out
}

// Failover: every probe cycle each remote of a failover rule is dialled from
// its node; two failures mark it down, two successes bring it back.
var fwdHealth struct {
	sync.Mutex
	fails, oks map[string]int
}

func (s *Server) probeForwards() {
	var rs []store.ForwardRule
	s.st.DB.Where("enabled = ? AND failover = ?", true, true).Find(&rs)
	fwdHealth.Lock()
	if fwdHealth.fails == nil {
		fwdHealth.fails, fwdHealth.oks = map[string]int{}, map[string]int{}
	}
	fwdHealth.Unlock()
	for i := range rs {
		r := &rs[i]
		if len(r.Remotes.V) < 2 || !s.hub.Online(r.NodeID) {
			continue
		}
		health := map[string]bool{}
		for k, v := range r.Health.V {
			health[k] = v
		}
		changed := false
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, rm := range r.Remotes.V {
			if !rm.Enabled {
				continue
			}
			wg.Add(1)
			go func(addr string) {
				defer wg.Done()
				res, err := s.hub.Command(s.ctxBG(), r.NodeID, "probe", map[string]string{"tcp": addr}, 12*time.Second)
				ok := err == nil && res.Ok
				key := r.ID + "|" + addr
				fwdHealth.Lock()
				if ok {
					fwdHealth.oks[key]++
					fwdHealth.fails[key] = 0
				} else {
					fwdHealth.fails[key]++
					fwdHealth.oks[key] = 0
				}
				f, o := fwdHealth.fails[key], fwdHealth.oks[key]
				fwdHealth.Unlock()
				mu.Lock()
				defer mu.Unlock()
				switch {
				case !health[addr] && f >= 2:
					health[addr], changed = true, true
				case health[addr] && o >= 2:
					delete(health, addr)
					changed = true
				}
			}(rm.Addr)
		}
		wg.Wait()
		if changed {
			s.st.DB.Model(r).Update("health", store.NewJSON(health))
			s.MarkDirty("forward "+r.Name+" failover", r.NodeID)
			s.log.Info("forward failover", "rule", r.Name, "down", health)
		}
	}
}
