package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
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
	if !force && node.DesiredRev > 0 {
		if prev, err := s.loadRevision(nodeID, node.DesiredRev); err == nil && sameSpec(prev, spec) {
			return node.DesiredRev, false, nil
		}
	}
	return s.storeRevision(&node, spec, reason)
}

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
		if err := tx.Model(&store.Node{}).Where("id = ?", node.ID).Updates(upd).Error; err != nil {
			return err
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
	spec := &model.NodeSpec{NodeID: node.ID, DefaultCore: node.DefaultCore, Inbounds: []model.Inbound{}}

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
	users, err := s.inboundUsers(inIDs)
	if err != nil {
		return nil, err
	}
	certCache := map[string]*store.Certificate{}
	spec.IPLimits = map[string]int{}
	for i := range ins {
		m := s.toModelInbound(&ins[i])
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
			m.Users = append(m.Users, u.ToModel())
			if u.IPLimit > 0 {
				spec.IPLimits[u.ID] = u.IPLimit
			}
		}
		sort.Slice(m.Users, func(a, b int) bool { return m.Users[a].ID < m.Users[b].ID })
		spec.Inbounds = append(spec.Inbounds, m)
	}

	if spec.Outbounds, err = s.buildOutbounds(node.ID); err != nil {
		return nil, err
	}
	if spec.Routes, err = s.buildRoutes(node.ID); err != nil {
		return nil, err
	}
	if spec.Forwards, err = s.buildForwards(node.ID); err != nil {
		return nil, err
	}
	if len(spec.Forwards) > 0 && !spec.HasCore(model.CoreForwarder) {
		spec.Cores = append(spec.Cores, model.CoreSpec{Type: model.CoreForwarder})
	}
	return spec, nil
}

// inboundUsers maps inbound id -> authorised users: active users granted via
// groups, plus service accounts of outbounds/chains that target the inbound.
func (s *Server) inboundUsers(inboundIDs []string) (map[string][]store.User, error) {
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
		Select("DISTINCT group_inbounds.inbound_id AS inbound_id, users.*").
		Joins("JOIN user_groups ON user_groups.user_id = users.id").
		Joins("JOIN group_inbounds ON group_inbounds.group_id = user_groups.group_id").
		Where("group_inbounds.inbound_id IN ? AND users.status = ? AND users.is_service = ? AND (users.banned_until IS NULL OR users.banned_until < ?)",
			inboundIDs, "active", false, now).
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
	set := model.OutboundSettings{Server: nodeHost(&target), Port: m.Port}
	var typ model.OutboundType
	switch m.Protocol.Type {
	case model.ProtoVLESS:
		typ, set.UUID, set.Flow = model.OutVLESS, svc.UUID, m.Protocol.Flow
	case model.ProtoVMess:
		typ, set.UUID = model.OutVMess, svc.UUID
	case model.ProtoTrojan:
		typ, set.Password = model.OutTrojan, svc.Password
	case model.ProtoShadowsocks:
		typ, set.Method = model.OutShadowsocks, m.Protocol.Method
		u := svc.ToModel()
		set.Password = u.SSKey(m.Protocol.Method)
		if model.SS2022KeyLen(m.Protocol.Method) > 0 {
			set.Password = m.Protocol.ServerKey + ":" + set.Password
		}
	case model.ProtoHysteria2:
		typ, set.Password, set.ObfsPassword = model.OutHysteria2, svc.Password, m.Protocol.ObfsPassword
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
		out = append(out, model.RouteRule{ID: r.ID, NodeID: nodeID, Match: m, Outbound: r.OutboundTag, Priority: r.Priority})
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
		hops := c.Hops.V
		if len(hops) < 2 {
			continue
		}
		landing := hops[len(hops)-1]
		var in store.Inbound
		if err := s.st.DB.First(&in, "id = ?", landing.InboundID).Error; err != nil {
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
				Target: net.JoinHostPort(nodeHost(&nn), strconv.Itoa(port)),
			})
		}
	}
	return out, nil
}

// nodeHost is the address other nodes and clients use to reach a node.
func nodeHost(n *store.Node) string {
	if n.Address != "" {
		return n.Address
	}
	return n.Domain
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
		Joins("JOIN group_inbounds ON group_inbounds.inbound_id = inbounds.id").
		Joins("JOIN user_groups ON user_groups.group_id = group_inbounds.group_id").
		Where("user_groups.user_id IN ?", userIDs).Distinct().Pluck("inbounds.node_id", &ids)
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
