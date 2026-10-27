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
	Username string `json:"username,omitempty"` // owner of a self-service rule
}

func (s *Server) listForwards(c *gin.Context) {
	var rs []store.ForwardRule
	q := s.st.DB.Order("node_id, listen_port")
	if n := c.Query("node"); n != "" {
		q = q.Where("node_id = ?", n)
	}
	if u := c.Query("user"); u != "" {
		q = q.Where("user_id = ?", u)
	}
	q.Find(&rs)
	names := s.nodeNames()
	owners := map[string]string{}
	var uids []string
	for _, r := range rs {
		if r.UserID != "" {
			uids = append(uids, r.UserID)
		}
	}
	if len(uids) > 0 {
		var us []store.User
		s.st.DB.Select("id", "username").Where("id IN ?", uids).Find(&us)
		for _, u := range us {
			owners[u.ID] = u.Username
		}
	}
	out := make([]forwardView, 0, len(rs))
	for _, r := range rs {
		out = append(out, forwardView{r, names[r.NodeID], owners[r.UserID]})
	}
	c.JSON(http.StatusOK, out)
}

type forwardReq struct {
	NodeID      string                `json:"nodeId" binding:"required"`
	Engine      string                `json:"engine"`
	TunnelAuth  string                `json:"tunnelAuth"`
	RateIn      int                   `json:"rateIn"`
	RateOut     int                   `json:"rateOut"`
	MaxConns    int                   `json:"maxConns"`
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
	gost := r.Engine == model.EngineGost
	if r.Engine != "" && !gost {
		return fmt.Errorf("转发引擎应为 realm 或 gost")
	}
	switch r.TunnelRole {
	case "":
		r.TunnelType, r.TunnelAuth = "", ""
	case "client", "server":
		types := map[string]bool{"ws": true, "tls": true, "wss": true}
		if gost {
			types = map[string]bool{"ws": true, "tls": true, "wss": true, "mwss": true, "mtls": true, "kcp": true, "quic": true, "grpc": true}
		} else if r.Network == "udp" {
			return fmt.Errorf("realm 的加密隧道只支持 TCP（GOST 隧道可同时转发 UDP）")
		}
		if !types[r.TunnelType] {
			return fmt.Errorf("不支持的隧道类型 %q", r.TunnelType)
		}
	default:
		return fmt.Errorf("隧道角色应为 client / server")
	}
	if gost {
		r.SendProxy, r.Through = 0, "" // realm only
	} else {
		r.TunnelAuth, r.RateIn, r.RateOut, r.MaxConns = "", 0, 0, 0
	}
	if r.RateIn < 0 || r.RateOut < 0 || r.MaxConns < 0 {
		return fmt.Errorf("限速与连接数不能为负")
	}
	if r.SendProxy < 0 || r.SendProxy > 2 {
		return fmt.Errorf("PROXY protocol 版本应为 1 或 2")
	}
	return nil
}

func (r *forwardReq) apply(row *store.ForwardRule) {
	row.NodeID, row.Name, row.ListenIP, row.ListenPort, row.Network = r.NodeID, strings.TrimSpace(r.Name), r.ListenIP, r.ListenPort, r.Network
	row.Engine, row.TunnelAuth, row.RateIn, row.RateOut, row.MaxConns = r.Engine, r.TunnelAuth, r.RateIn, r.RateOut, r.MaxConns
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
	ln := listenNet(r.Engine, r.TunnelRole, r.TunnelType, r.Network)
	// A stand-in inbound with the same networks for portConflict.
	switch ln {
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
		if netOverlap(listenNet(o.Engine, o.TunnelRole, o.TunnelType, o.Network), ln) {
			return fmt.Errorf("端口 %d 已被转发规则 %s 使用", r.ListenPort, o.Name)
		}
	}
	return nil
}

// listenNet is the network a forward rule's listening socket uses.
func listenNet(engine, role, ttype, network string) string {
	f := model.ForwardRule{Engine: engine, TunnelRole: role, TunnelType: ttype, Network: network}
	return f.ListenNetwork()
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
	s.st.DB.Where("node_id = ? AND kind = ? AND tag = ?", row.NodeID, "inbound", "fwd-"+row.ID).Delete(&store.TagTraffic{})
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
	rs, userKBps := s.userForwardAllowed(rs)
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
			if r.Failover && r.Engine != model.EngineGost && r.Health.V[rm.Addr] { // GOST fails over natively
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
		if r.Engine == model.EngineGost {
			f.Engine, f.Failover, f.TunnelRole, f.TunnelType = model.EngineGost, r.Failover, r.TunnelRole, r.TunnelType
			f.TunnelHost, f.TunnelPath, f.TunnelSNI, f.TunnelAuth = r.TunnelHost, r.TunnelPath, r.TunnelSNI, r.TunnelAuth
			f.RateIn, f.RateOut, f.MaxConns = r.RateIn, r.RateOut, r.MaxConns
			if r.UserID != "" {
				f.RateIn, f.RateOut = userKBps[r.ID], userKBps[r.ID] // the plan's cap, as it is now
			}
			out = append(out, f)
			continue
		}
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
	s.st.DB.Where("enabled = ? AND failover = ? AND (engine IS NULL OR engine <> ?)", true, true, model.EngineGost).Find(&rs)
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
			s.warnWrite("forward health", s.st.DB.Model(r).Update("health", store.NewJSON(health)).Error)
			s.MarkDirty("forward "+r.Name+" failover", r.NodeID)
			s.log.Info("forward failover", "rule", r.Name, "down", health)
		}
	}
}
