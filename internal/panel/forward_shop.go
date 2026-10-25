package panel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Self-service port forwarding (professional edition, plan add-on): a plan
// grants a number of GOST forward rules on the nodes the user can reach,
// within a port range, with its own traffic allowance per period. Users
// manage them in the user center. GOST (not realm) so the traffic can be
// metered per rule and speed-limited.

const (
	fwdDefaultPortLo = 10000
	fwdDefaultPortHi = 60000
	fwdMaxPerUser    = 100
)

var errFwdTarget = errors.New("目标地址必须是公网地址，不能指向内网、本机或面板管理的节点")

func validFwdEntitlement(e *store.ForwardEntitlement) error {
	switch {
	case e.FwdMax < 0 || e.FwdMax > fwdMaxPerUser:
		return fmt.Errorf("转发条数应在 0–%d 之间", fwdMaxPerUser)
	case e.FwdTraffic < 0 || e.FwdSpeedMbps < 0 || e.FwdSpeedMbps > 100000:
		return errors.New("转发流量和限速不能为负")
	case e.FwdRate < 0 || e.FwdRate > 100:
		return errors.New("转发倍率应在 0–100 之间（0 = 1 倍）")
	case e.FwdPortLo != 0 || e.FwdPortHi != 0:
		if e.FwdPortLo < 1024 || e.FwdPortHi > 65535 || e.FwdPortLo > e.FwdPortHi {
			return errors.New("转发端口范围应在 1024–65535 之间，且起始不大于结束")
		}
	}
	return nil
}

// fwdPorts is the user's listen port range.
func fwdPorts(e store.ForwardEntitlement) (int, int) {
	if e.FwdPortLo == 0 && e.FwdPortHi == 0 {
		return fwdDefaultPortLo, fwdDefaultPortHi
	}
	return e.FwdPortLo, e.FwdPortHi
}

func fwdRate(e store.ForwardEntitlement) float64 {
	if e.FwdRate <= 0 {
		return 1
	}
	return e.FwdRate
}

// fwdServes reports whether u's rules are delivered: the feature is
// licensed, the account is active and the forwarding allowance is left.
func (s *Server) fwdServes(u *store.User) bool {
	return s.license().Allow(license.FeaturePlanAddons) && u.Status == "active" && u.FwdMax > 0 &&
		!u.FwdCapped && (u.FwdTraffic == 0 || u.FwdUsed < u.FwdTraffic)
}

// fwdNodes are the nodes u may forward on: those hosting an inbound u can use.
func (s *Server) fwdNodes(userID string) []store.Node {
	var ids []string
	s.st.DB.Table("inbounds").Joins("JOIN ("+store.GrantsSQL+") grants ON grants.inbound_id = inbounds.id").
		Where("grants.user_id = ?", userID).Distinct().Pluck("inbounds.node_id", &ids)
	var nodes []store.Node
	if len(ids) > 0 {
		s.st.DB.Where("id IN ? AND revoked = ?", ids, false).Order("name").Find(&nodes)
	}
	return nodes
}

// fwdTarget resolves a user's target to one public IP:port. The address is
// pinned at creation: a later DNS change cannot turn it inward.
func (s *Server) fwdTarget(ctx context.Context, addr string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || host == "" {
		return "", errors.New("目标应写成 地址:端口，例如 example.com:443")
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return "", errors.New("目标端口无效")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return "", fmt.Errorf("无法解析 %s", host)
		}
		for _, x := range ips { // prefer IPv4: every node has it
			if x.To4() != nil {
				ip = x
				break
			}
		}
		if ip == nil {
			ip = ips[0]
		}
	}
	if checkSourceIP(ip, false) != nil || ip.Equal(net.IPv4bcast) {
		return "", errFwdTarget
	}
	// Not into the fleet itself: node and panel services are not a product.
	var nodes []store.Node
	s.st.DB.Select("address", "domain", "public_ipv4", "public_ipv6").Find(&nodes)
	for _, n := range nodes {
		for _, h := range []string{n.Address, n.PublicIPv4, n.PublicIPv6} {
			if x := net.ParseIP(strings.TrimSpace(h)); x != nil && x.Equal(ip) {
				return "", errFwdTarget
			}
		}
	}
	return net.JoinHostPort(ip.String(), port), nil
}

type userForwardReq struct {
	NodeID     string `json:"nodeId"`
	Network    string `json:"network"`
	Target     string `json:"target"`
	ListenPort int    `json:"listenPort"` // 0 = pick a free one in range
	Name       string `json:"name"`
}

// createUserForward adds a rule for u after the entitlement checks.
func (s *Server) createUserForward(ctx context.Context, u *store.User, req userForwardReq) (*store.ForwardRule, error) {
	if !s.license().Allow(license.FeaturePlanAddons) || u.FwdMax <= 0 {
		return nil, errors.New("你的套餐不包含端口转发")
	}
	if u.Status != "active" {
		return nil, errors.New("账号未处于正常状态，暂时不能添加转发")
	}
	s.relayMu.Lock() // serialises port allocation with the admin paths
	defer s.relayMu.Unlock()
	var n int64
	s.st.DB.Model(&store.ForwardRule{}).Where("user_id = ?", u.ID).Count(&n)
	if int(n) >= u.FwdMax {
		return nil, fmt.Errorf("最多 %d 条转发，已用完", u.FwdMax)
	}
	allowed := false
	for _, nd := range s.fwdNodes(u.ID) {
		allowed = allowed || nd.ID == req.NodeID
	}
	if !allowed {
		return nil, errors.New("这个节点不在你的套餐里")
	}
	target, err := s.fwdTarget(ctx, req.Target)
	if err != nil {
		return nil, err
	}
	lo, hi := fwdPorts(u.ForwardEntitlement)
	if req.ListenPort == 0 {
		if req.ListenPort = s.randomFreePort(req.NodeID, lo, hi); req.ListenPort == 0 {
			return nil, errors.New("这个节点的可用端口已用完，请换一个节点")
		}
	} else if req.ListenPort < lo || req.ListenPort > hi {
		return nil, fmt.Errorf("端口应在 %d–%d 之间", lo, hi)
	}
	fr := forwardReq{NodeID: req.NodeID, Engine: model.EngineGost, Network: req.Network, ListenPort: req.ListenPort,
		Name: truncate(strings.TrimSpace(req.Name), 64), Remotes: []store.ForwardRemote{{Addr: target, Enabled: true, Weight: 1}}}
	if err := fr.validate(); err != nil {
		return nil, err
	}
	if err := s.forwardPortConflict(&fr, ""); err != nil {
		return nil, errors.New("这个端口已被占用，换一个或留空自动分配")
	}
	row := store.ForwardRule{ID: store.NewID(), UserID: u.ID, Enabled: true, Health: store.NewJSON(map[string]bool{})}
	fr.apply(&row)
	if row.Name == "" || strings.HasPrefix(row.Name, "fwd-") {
		row.Name = fmt.Sprintf("%s-%d", u.Username, row.ListenPort)
	}
	if err := s.st.DB.Create(&row).Error; err != nil {
		return nil, err
	}
	s.MarkDirty("user forward created", row.NodeID)
	return &row, nil
}

func (s *Server) deleteUserForward(u *store.User, id string) error {
	var row store.ForwardRule
	if s.st.DB.First(&row, "id = ? AND user_id = ?", id, u.ID).Error != nil {
		return errors.New("转发不存在")
	}
	s.st.DB.Delete(&row)
	s.st.DB.Where("node_id = ? AND kind = ? AND tag = ?", row.NodeID, "inbound", "fwd-"+row.ID).Delete(&store.TagTraffic{})
	s.MarkDirty("user forward deleted", row.NodeID)
	return nil
}

// userForwardAllowed filters a node's rules: a self-service rule is
// delivered only while its owner is served and within FwdMax (the oldest
// ones count first when a plan shrinks). kbps holds each kept self-service
// rule's speed cap in KB/s (0 = none).
func (s *Server) userForwardAllowed(rs []store.ForwardRule) (out []store.ForwardRule, kbps map[string]int) {
	kbps = map[string]int{}
	owners := map[string]*store.User{}
	for _, r := range rs {
		if r.UserID != "" {
			owners[r.UserID] = nil
		}
	}
	if len(owners) == 0 {
		return rs, kbps
	}
	ids := make([]string, 0, len(owners))
	for id := range owners {
		ids = append(ids, id)
	}
	var us []store.User
	s.st.DB.Where("id IN ?", ids).Find(&us)
	keep := map[string]bool{} // rule ID
	for i := range us {
		u := &us[i]
		if !s.fwdServes(u) {
			continue
		}
		var mine []store.ForwardRule
		s.st.DB.Select("id").Where("user_id = ?", u.ID).Order("created_at, id").Limit(u.FwdMax).Find(&mine)
		for _, r := range mine {
			keep[r.ID] = true
			kbps[r.ID] = fwdSpeedKBps(u.FwdSpeedMbps)
		}
	}
	for _, r := range rs {
		if r.UserID == "" || keep[r.ID] {
			out = append(out, r)
		}
	}
	return out, kbps
}

// fwdSpeedKBps converts the plan's per-rule Mbps to GOST's KB/s.
func fwdSpeedKBps(mbps int) int { return mbps * 125 }

// userForwardNodes lists the nodes hosting the users' forward rules.
func (s *Server) userForwardNodes(userIDs ...string) []string {
	var ids []string
	if len(userIDs) > 0 {
		s.st.DB.Model(&store.ForwardRule{}).Where("user_id IN ?", userIDs).Distinct().Pluck("node_id", &ids)
	}
	return ids
}
