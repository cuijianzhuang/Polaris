package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// The panel keeps only the desired state of every node ("what it should look
// like"); agents converge onto it. Every write marks affected nodes dirty, the
// reconciler debounces and publishes a new revision when the spec changed.

// MarkDirty schedules spec rebuilds for nodes.
func (s *Server) MarkDirty(reason string, nodeIDs ...string) {
	s.dirtyMu.Lock()
	for _, id := range nodeIDs {
		if id != "" {
			s.dirty[id] = reason
		}
	}
	s.dirtyMu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// MarkAllDirty rebuilds every node (e.g. after a user or group change whose
// reach is expensive to compute precisely).
func (s *Server) MarkAllDirty(reason string) {
	var ids []string
	s.st.DB.Model(&store.Node{}).Pluck("id", &ids)
	s.MarkDirty(reason, ids...)
}

func (s *Server) runReconciler(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
		}
		time.Sleep(400 * time.Millisecond) // debounce bursts of writes
		s.dirtyMu.Lock()
		batch := s.dirty
		s.dirty = map[string]string{}
		s.dirtyMu.Unlock()
		for id, reason := range batch {
			if _, _, err := s.Publish(id, reason, false); err != nil {
				s.log.Error("publish spec", "node", id, "err", err)
			}
		}
	}
}

// Publish builds the node's spec and stores a new revision if it changed (or
// force is set), then notifies the agent.
func (s *Server) Publish(nodeID, reason string, force bool) (int64, bool, error) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	spec, err := s.BuildSpec(nodeID)
	if err != nil {
		return 0, false, err
	}
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", nodeID).Error; err != nil {
		return 0, false, err
	}
	for attempt := 0; ; attempt++ {
		if !force && node.DesiredRev > 0 {
			if prev, err := s.loadRevision(nodeID, node.DesiredRev); err == nil && sameSpec(prev, spec) {
				return node.DesiredRev, false, nil
			}
		}
		rev, changed, err := s.storeRevision(&node, spec, reason)
		if !errors.Is(err, errRevisionRace) && !store.IsDuplicate(err) || attempt == 20 {
			return rev, changed, err
		}
		time.Sleep(time.Duration(rand.IntN(20)+1) * time.Millisecond) // let the other instance finish
		// Another instance stored a revision first: compare against it.
		if err := s.st.DB.First(&node, "id = ?", nodeID).Error; err != nil {
			return 0, false, err
		}
	}
}

// errRevisionRace means another instance moved the node's revision first.
var errRevisionRace = errors.New("node revision changed concurrently")

func (s *Server) storeRevision(node *store.Node, spec *model.NodeSpec, reason string) (int64, bool, error) {
	rev := node.DesiredRev + 1
	spec.Rev = rev
	spec.CreatedAt = time.Now().UTC()
	b, err := json.Marshal(spec)
	if err != nil {
		return 0, false, err
	}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&store.ConfigRevision{NodeID: node.ID, Rev: rev, Spec: s.st.Box.Seal(string(b)),
			Reason: truncate(reason, 250), Status: "pending"}).Error; err != nil {
			return err
		}
		upd := map[string]any{"desired_rev": rev}
		if node.Status == "online" {
			upd["status"] = "syncing"
		}
		// Conditional: another panel instance publishing the same node at
		// the same moment makes this one retry with the next number.
		r := tx.Model(&store.Node{}).Where("id = ? AND desired_rev = ?", node.ID, node.DesiredRev).Updates(upd)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 0 {
			return errRevisionRace
		}
		// Keep the most recent 50 revisions.
		return tx.Where("node_id = ? AND rev <= ?", node.ID, rev-50).Delete(&store.ConfigRevision{}).Error
	})
	if err != nil {
		return 0, false, err
	}
	s.hub.NotifySpec(node.ID, rev)
	s.log.Info("spec published", "node", node.Name, "rev", rev, "reason", reason)
	return rev, true, nil
}

// loadRevision returns a stored spec.
func (s *Server) loadRevision(nodeID string, rev int64) (*model.NodeSpec, error) {
	var r store.ConfigRevision
	if err := s.st.DB.Where("node_id = ? AND rev = ?", nodeID, rev).First(&r).Error; err != nil {
		return nil, err
	}
	raw, err := s.st.Box.Open(r.Spec)
	if err != nil {
		return nil, err
	}
	var spec model.NodeSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func sameSpec(a, b *model.NodeSpec) bool {
	x, y := a.Clone(), b.Clone()
	x.Rev, y.Rev = 0, 0
	x.CreatedAt, y.CreatedAt = time.Time{}, time.Time{}
	bx, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(bx, by)
}

// BuildSpec assembles the complete desired state of a node from the database.
func (s *Server) BuildSpec(nodeID string) (*model.NodeSpec, error) {
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", nodeID).Error; err != nil {
		return nil, err
	}
	spec := &model.NodeSpec{NodeID: node.ID, DefaultCore: node.DefaultCore, Inbounds: []model.Inbound{}, AutoFirewall: !node.NoFirewall,
		Geodata: s.geodataSpec(), DNS: s.dnsSpec(nodeID)}

	cores, err := s.nodeCores(node.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range cores {
		spec.Cores = append(spec.Cores, model.CoreSpec{Type: c.Type, Version: c.Version})
	}

	var ins []store.Inbound
	if err := s.st.DB.Where("node_id = ?", node.ID).Order("port, tag").Find(&ins).Error; err != nil {
		return nil, err
	}
	inIDs := make([]string, 0, len(ins))
	for _, i := range ins {
		inIDs = append(inIDs, i.ID)
	}
	capped, packIDs := s.nodeQuotaUsers(node.ID)
	users, err := s.inboundUsers(inIDs, packIDs...)
	if err != nil {
		return nil, err
	}
	certCache := map[string]*store.Certificate{}
	viaCF := s.cloudflareInbounds(inIDs)
	spec.IPLimits = map[string]int{}
	limited := map[string]store.User{} // candidates for per-user speed limits
	for i := range ins {
		m := s.toModelInbound(&ins[i])
		if ins[i].RelayAccess.V.Enabled && ins[i].RelayAccess.V.ProxyProtocol > 0 {
			m.Transport.AcceptProxyProtocol = true
		}
		if viaCF[m.ID] {
			// Cloudflare adds CF-Connecting-IP and puts the visitor first in
			// X-Forwarded-For: count users by that, not by the edge address.
			m.Transport.TrustedForwardedFor = []string{"CF-Connecting-IP"}
		}
		if m.Security.Type == model.SecTLS && m.Security.CertificateID != "" {
			c, ok := certCache[m.Security.CertificateID]
			if !ok {
				c = &store.Certificate{}
				if err := s.st.DB.First(c, "id = ?", m.Security.CertificateID).Error; err != nil {
					c = nil
				}
				certCache[m.Security.CertificateID] = c
			}
			if c != nil {
				m.Security.CertPEM = c.Cert
				m.Security.KeyPEM = s.st.Box.MustOpen(c.Key)
			}
		}
		for _, u := range users[m.ID] {
			if capped[u.ID] {
				continue // per-node cap reached
			}
			m.Users = append(m.Users, u.ToModel())
			if u.IPLimit > 0 {
				spec.IPLimits[u.ID] = u.IPLimit
			}
		}
		sort.Slice(m.Users, func(a, b int) bool { return m.Users[a].ID < m.Users[b].ID })
		if m.Core != model.CoreHysteria { // Hysteria2 cannot route by user
			for _, u := range users[m.ID] {
				if !capped[u.ID] {
					limited[u.ID] = u
				}
			}
		}
		if ins[i].SpeedLimitMbps > 0 && m.Enabled && m.Port > 0 && !strings.HasPrefix(m.Listen, "127.") {
			spec.SpeedLimits = append(spec.SpeedLimits, model.SpeedLimit{Tag: m.Tag, Port: m.Port, Network: m.Network(), Mbps: ins[i].SpeedLimitMbps})
		}
		spec.Inbounds = append(spec.Inbounds, m)
	}
	resolveFallbacks(spec.Inbounds)
	if spec.AccessRules, err = s.buildAccessRules(node.ID); err != nil {
		return nil, err
	}
	spec.UserLimits = s.userLimits(limited)

	if spec.Outbounds, err = s.buildOutbounds(node.ID); err != nil {
		return nil, err
	}
	if spec.Routes, err = s.buildRoutes(node.ID); err != nil {
		return nil, err
	}
	collapseBlockedSourcePaths(spec)
	if spec.Forwards, err = s.buildForwards(node.ID); err != nil {
		return nil, err
	}
	if len(spec.GostForwards()) > 0 && !spec.HasCore(model.CoreGost) {
		spec.Cores = append(spec.Cores, model.CoreSpec{Type: model.CoreGost})
	}
	if len(spec.RealmForwards()) > 0 && !spec.HasCore(model.CoreForwarder) {
		spec.Cores = append(spec.Cores, model.CoreSpec{Type: model.CoreForwarder})
	}
	spec.Fronts = s.buildFronts(spec)
	if len(spec.Fronts) > 0 && !spec.HasCore(model.CoreCaddy) {
		spec.Cores = append(spec.Cores, model.CoreSpec{Type: model.CoreCaddy})
	}
	return spec, nil
}

// inboundUsers maps inbound id -> authorised users: active users granted via
// groups or direct node / inbound bindings, plus service accounts of outbounds/chains that target the inbound.
// packIDs are limited users still served by a node traffic pack.
func (s *Server) inboundUsers(inboundIDs []string, packIDs ...string) (map[string][]store.User, error) {
	out := map[string][]store.User{}
	if len(inboundIDs) == 0 {
		return out, nil
	}
	type row struct {
		InboundID string
		store.User
	}
	var rows []row
	now := time.Now()
	err := s.st.DB.Table("users").
		Select("DISTINCT grants.inbound_id AS inbound_id, users.*").
		Joins("JOIN ("+store.GrantsSQL+") grants ON grants.user_id = users.id").
		Where("grants.inbound_id IN ? AND (users.status = ? OR (users.status = ? AND users.id IN ?)) AND users.is_service = ? AND (users.banned_until IS NULL OR users.banned_until < ?)",
			inboundIDs, "active", "limited", append(packIDs, ""), false, now).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.InboundID] = append(out[r.InboundID], r.User)
	}
	// Service accounts used by node_ref outbounds pointing at these inbounds.
	var refs []store.Outbound
	if err := s.st.DB.Where("node_ref_inbound_id IN ? AND service_user_id <> ''", inboundIDs).Find(&refs).Error; err != nil {
		return nil, err
	}
	for _, o := range refs {
		var u store.User
		if err := s.st.DB.First(&u, "id = ?", o.ServiceUserID).Error; err == nil {
			out[o.NodeRefInboundID] = appendUnique(out[o.NodeRefInboundID], u)
		}
	}
	var dedicated []store.Inbound
	if err := s.st.DB.Where("id IN ?", inboundIDs).Find(&dedicated).Error; err != nil {
		return nil, err
	}
	for _, in := range dedicated {
		if in.Protocol.V.Type != model.ProtoSnell || in.Protocol.V.SnellAuth != "psk" {
			continue
		}
		users := out[in.ID]
		out[in.ID] = nil
		for _, u := range users {
			if u.ID == in.Protocol.V.SnellUserID {
				out[in.ID] = append(out[in.ID], u)
			}
		}
	}
	return out, nil
}

func appendUnique(us []store.User, u store.User) []store.User {
	for _, x := range us {
		if x.ID == u.ID {
			return us
		}
	}
	return append(us, u)
}

// buildOutbounds resolves node_ref outbounds into concrete client outbounds.
func (s *Server) buildOutbounds(nodeID string) ([]model.Outbound, error) {
	var obs []store.Outbound
	if err := s.st.DB.Where("node_id = ?", nodeID).Order("created_at").Find(&obs).Error; err != nil {
		return nil, err
	}
	var out []model.Outbound
	for _, o := range obs {
		m := model.Outbound{ID: o.ID, NodeID: o.NodeID, Tag: o.Tag, Type: o.Type, Settings: o.Settings, Detour: o.Detour}
		if o.Type == model.OutSourceRef {
			m = s.resolveSourceRef(o)
		}
		if o.Type == model.OutNodeRef {
			r, err := s.resolveNodeRef(&o)
			if err != nil {
				return nil, fmt.Errorf("outbound %s: %w", o.Tag, err)
			}
			m = *r
		}
		out = append(out, m)
	}
	return s.filterGroupMembers(obs, out), nil
}

// resolveNodeRef turns a reference to another node's inbound into a concrete
// outbound, filling address and service-account credentials.
func (s *Server) resolveNodeRef(o *store.Outbound) (*model.Outbound, error) {
	var in store.Inbound
	if err := s.st.DB.First(&in, "id = ?", o.NodeRefInboundID).Error; err != nil {
		return nil, fmt.Errorf("referenced inbound: %w", err)
	}
	var target store.Node
	if err := s.st.DB.First(&target, "id = ?", in.NodeID).Error; err != nil {
		return nil, err
	}
	var svc store.User
	if err := s.st.DB.First(&svc, "id = ?", o.ServiceUserID).Error; err != nil {
		return nil, fmt.Errorf("service account: %w", err)
	}
	m := s.toModelInbound(&in)
	if master := s.fallbackMasters([]string{in.NodeID})[in.ID]; master != nil {
		// Fallback child: reachable only through the master's port and TLS.
		mm := s.toModelInbound(master)
		m.Port, m.Security = mm.Port, mm.Security
	}
	set := model.OutboundSettings{Server: nodeHost(&target), Port: m.Port}
	var typ model.OutboundType
	switch m.Protocol.Type {
	case model.ProtoVLESS:
		typ, set.UUID, set.Flow, set.Encryption = model.OutVLESS, svc.UUID, m.Protocol.Flow, m.Protocol.Encryption
	case model.ProtoVMess:
		typ, set.UUID = model.OutVMess, svc.UUID
	case model.ProtoTrojan:
		typ, set.Password = model.OutTrojan, svc.Password
	case model.ProtoShadowsocks:
		typ, set.Method = model.OutShadowsocks, m.Protocol.Method
		u := svc.ToModel()
		set.Password = ssClientPassword(&m, &u)
	case model.ProtoHysteria2:
		typ, set.Password, set.ObfsPassword = model.OutHysteria2, svc.Password, m.Protocol.ObfsPassword
		if m.Protocol.Obfs == "" {
			set.ObfsPassword = ""
		}
	case model.ProtoSOCKS:
		typ, set.Username, set.Password = model.OutSOCKS, svc.ID, svc.Password
	case model.ProtoHTTP:
		typ, set.Username, set.Password = model.OutHTTP, svc.ID, svc.Password
	default:
		return nil, fmt.Errorf("protocol %s cannot be used as a chain hop", m.Protocol.Type)
	}
	t := m.Transport
	if t.Type != model.TransTCP && t.Type != model.TransQUIC {
		set.Transport = &t
	}
	if m.Security.Type != model.SecNone {
		ct := &model.ClientTLS{Type: m.Security.Type, ServerName: m.Security.ServerName, ALPN: m.Security.ALPN,
			Insecure: s.tlsInsecure(m.Security.CertificateID)}
		if m.Security.Type == model.SecReality {
			ct.ServerName = first(m.Security.RealityServerNames)
			ct.PublicKey = m.Security.RealityPublicKey
			ct.ShortID = first(m.Security.RealityShortIDs)
			ct.Fingerprint = "chrome"
			ct.SpiderX, ct.Mldsa65Verify = m.Security.RealitySpiderX, m.Security.RealityMldsa65Verify
		}
		set.Security = ct
	}
	b, _ := json.Marshal(set)
	return &model.Outbound{ID: o.ID, NodeID: o.NodeID, Tag: o.Tag, Type: typ, Settings: b, Detour: o.Detour}, nil
}

// buildRoutes resolves user-group conditions into user ids.
func (s *Server) buildRoutes(nodeID string) ([]model.RouteRule, error) {
	var rs []store.RoutingRule
	if err := s.st.DB.Where("node_id IN ?", []string{nodeID, model.GlobalNodeID}).Order("priority").Find(&rs).Error; err != nil {
		return nil, err
	}
	var tags []string
	s.st.DB.Model(&store.Outbound{}).Where("node_id = ?", nodeID).Pluck("tag", &tags)
	have := map[string]bool{"direct": true, "block": true}
	for _, t := range tags {
		have[t] = true
	}
	var out []model.RouteRule
	var sets map[string]*store.RuleSet
	for _, r := range rs {
		if len(r.Match.V.RuleSets) > 0 {
			sets = s.ruleSetsByTag()
			break
		}
	}
	// Inbounds bound to an egress come first.
	var ins []store.Inbound
	s.st.DB.Where("node_id = ? AND outbound_tag <> ''", nodeID).Find(&ins)
	for _, in := range ins {
		if have[in.OutboundTag] {
			out = append(out, model.RouteRule{ID: "inbound-" + in.ID, NodeID: nodeID, Priority: -1000,
				Match: model.RouteMatch{Inbound: []string{in.Tag}}, Outbound: in.OutboundTag})
		}
	}
	for _, r := range rs {
		if r.NodeID == model.GlobalNodeID && !have[r.OutboundTag] {
			continue // global rule targeting an egress this node does not have
		}
		m := r.Match.V
		if len(m.UserGroups) > 0 {
			var ids []string
			s.st.DB.Model(&store.UserGroup{}).Where("group_id IN ?", m.UserGroups).Distinct().Pluck("user_id", &ids)
			sort.Strings(ids)
			m.Users = append(m.Users, ids...)
		}
		out = append(out, expandRuleSets(model.RouteRule{ID: r.ID, NodeID: nodeID, Match: m, Outbound: r.OutboundTag, Priority: r.Priority}, sets)...)
	}
	sortRules(out)
	return out, nil
}

// buildForwards emits L4 forwards for relay hops of l4 chains on this node.
func (s *Server) buildForwards(nodeID string) ([]model.ForwardRule, error) {
	var chains []store.Chain
	if err := s.st.DB.Where("mode = ?", "l4").Find(&chains).Error; err != nil {
		return nil, err
	}
	var out []model.ForwardRule
	for _, c := range chains {
		if c.Disabled {
			continue
		}
		hops := c.Hops.V
		if len(hops) < 2 {
			continue
		}
		participates := false
		for _, h := range hops {
			participates = participates || h.NodeID == nodeID
		}
		if !participates {
			continue
		}
		landing := hops[len(hops)-1]
		var in store.Inbound
		if err := s.st.DB.First(&in, "id = ?", landing.InboundID).Error; err != nil {
			continue
		}
		if c.Managed && !in.Enabled {
			continue
		}
		if in.RelayAccess.V.Enabled {
			if _, e := checkRelayAccessChain(s.st.DB, in, &c); errors.Is(e, errRelayNodeGone) {
				continue
			} else if e != nil {
				return nil, e
			}
		}
		if c.Managed && c.TunnelType != "" {
			nodes, e := relayNodes(s.st.DB, &c)
			if errors.Is(e, errRelayNodeGone) {
				continue
			}
			if e != nil {
				return nil, e
			}
			auth, e := s.st.Box.Open(c.TunnelAuth)
			if e != nil || auth == "" {
				return nil, fmt.Errorf("managed relay %s: invalid tunnel authentication", c.ID)
			}
			out = append(out, managedRelayForwards(&c, nodes, in, auth)[nodeID]...)
			continue
		}
		mi := s.toModelInbound(&in)
		mi.Normalize()
		for i := 0; i < len(hops)-1; i++ {
			if hops[i].NodeID != nodeID {
				continue
			}
			next := hops[i+1]
			var nn store.Node
			if err := s.st.DB.First(&nn, "id = ?", next.NodeID).Error; err != nil {
				continue
			}
			port := next.Port
			if i+1 == len(hops)-1 {
				port = in.Port
			}
			out = append(out, model.ForwardRule{
				Tag: fmt.Sprintf("chain-%s-%d", c.ID, i), Listen: hops[i].Port, Network: mi.Network(),
				Target:    net.JoinHostPort(nodeHost(&nn), strconv.Itoa(port)),
				SendProxy: in.RelayAccess.V.ProxyProtocol, AcceptProxy: in.RelayAccess.V.ProxyProtocol > 0 && i > 0,
			})
		}
	}
	return append(out, s.standaloneForwards(nodeID)...), nil
}

// nodeHost is the address other nodes and clients use to reach a node.
// nodeHost is the address clients use for a node: the configured address,
// else its domain, else the public IP the agent detected (so a node added
// without an address still yields working links).
// nodeHost is the address clients and other nodes dial: the node's domain
// when it has one (it survives IP changes), else its address, else the
// IP the agent detected.
func nodeHost(n *store.Node) string {
	for _, h := range []string{n.Domain, n.Address, n.PublicIPv4, n.PublicIPv6} {
		if h = strings.TrimSpace(h); h != "" {
			return h
		}
	}
	return ""
}

// nodeOrigin is what a DNS record pointing at the node should hold: an IP
// when one is known (a CNAME to the node's own domain could loop back to
// the record being written), else nodeHost.
func nodeOrigin(n *store.Node) string {
	if ip := net.ParseIP(strings.TrimSpace(n.Address)); ip != nil {
		return ip.String()
	}
	for _, h := range []string{n.PublicIPv4, n.PublicIPv6} {
		if h = strings.TrimSpace(h); h != "" {
			return h
		}
	}
	return nodeHost(n)
}

// nodesForInbounds returns node ids hosting the inbounds plus nodes whose
// outbounds or chains reference them (dependency fan-out).
func (s *Server) nodesForInbounds(inboundIDs ...string) []string {
	set := map[string]bool{}
	var ins []store.Inbound
	s.st.DB.Where("id IN ?", inboundIDs).Find(&ins)
	for _, i := range ins {
		set[i.NodeID] = true
	}
	var refs []string
	s.st.DB.Model(&store.Outbound{}).Where("node_ref_inbound_id IN ?", inboundIDs).Pluck("node_id", &refs)
	for _, r := range refs {
		set[r] = true
	}
	var chains []store.Chain
	s.st.DB.Find(&chains)
	for _, c := range chains {
		for _, h := range c.Hops.V {
			for _, id := range inboundIDs {
				if h.InboundID == id {
					for _, hh := range c.Hops.V {
						set[hh.NodeID] = true
					}
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// nodesForUsers returns nodes hosting inbounds granted to the users.
func (s *Server) nodesForUsers(userIDs ...string) []string {
	var ids []string
	s.st.DB.Table("inbounds").
		Joins("JOIN ("+store.GrantsSQL+") grants ON grants.inbound_id = inbounds.id").
		Where("grants.user_id IN ?", userIDs).Distinct().Pluck("inbounds.node_id", &ids)
	ids = append(ids, s.userForwardNodes(userIDs...)...) // their self-service forwards follow the account
	// Routing rules may reference groups the users belong to.
	var gids []string
	s.st.DB.Model(&store.UserGroup{}).Where("user_id IN ?", userIDs).Pluck("group_id", &gids)
	if len(gids) > 0 {
		var rules []store.RoutingRule
		s.st.DB.Find(&rules)
		for _, r := range rules {
			for _, g := range r.Match.V.UserGroups {
				for _, x := range gids {
					if g == x {
						ids = append(ids, r.NodeID)
					}
				}
			}
		}
	}
	return ids
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// groupPlans returns, per user, the highest speed plan (up, down Mbps)
// among the user's groups.
func (s *Server) groupPlans(ids []string) map[string][2]int {
	plan := map[string][2]int{}
	if len(ids) == 0 {
		return plan
	}
	type row struct {
		UserID        string
		SpeedUpMbps   int
		SpeedDownMbps int
	}
	var rows []row
	s.st.DB.Table("user_groups").Select("user_groups.user_id, groups.speed_up_mbps, groups.speed_down_mbps").
		Joins("JOIN groups ON groups.id = user_groups.group_id").
		Where("user_groups.user_id IN ? AND (groups.speed_up_mbps > 0 OR groups.speed_down_mbps > 0)", ids).Scan(&rows)
	for _, r := range rows {
		p := plan[r.UserID]
		plan[r.UserID] = [2]int{max(p[0], r.SpeedUpMbps), max(p[1], r.SpeedDownMbps)}
	}
	return plan
}

// effectiveSpeed is the user's own rate per direction, else the plan's.
func effectiveSpeed(u *store.User, plan [2]int) (up, down int) {
	up, down = u.SpeedUpMbps, u.SpeedDownMbps
	if up <= 0 {
		up = plan[0]
	}
	if down <= 0 {
		down = plan[1]
	}
	return up, down
}

// cloudflareInbounds lists the inbounds among ids that have a CDN entry on a
// Cloudflare profile.
func (s *Server) cloudflareInbounds(ids []string) map[string]bool {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out
	}
	var rows []string
	s.st.DB.Table("endpoints").Select("DISTINCT endpoints.inbound_id").
		Joins("JOIN cdn_profiles ON cdn_profiles.id = endpoints.cdn_profile_id").
		Where("endpoints.kind = ? AND cdn_profiles.provider IN ? AND endpoints.inbound_id IN ?", "cdn", []string{"cloudflare", ""}, ids).
		Scan(&rows)
	for _, id := range rows {
		out[id] = true
	}
	return out
}
