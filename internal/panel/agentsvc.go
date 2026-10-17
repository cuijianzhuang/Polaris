package panel

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/api/gen/polaris/v1/polarisv1connect"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

type ctxKey int

const peerCertKey ctxKey = 1

// withPeerCert exposes the verified client certificate to handlers.
func withPeerCert(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			r = r.WithContext(context.WithValue(r.Context(), peerCertKey, r.TLS.PeerCertificates[0]))
		}
		h.ServeHTTP(w, r)
	})
}

type agentService struct {
	s *Server
}

var _ polarisv1connect.AgentServiceHandler = (*agentService)(nil)

// node authenticates the caller by its client certificate.
func (a *agentService) node(ctx context.Context) (*store.Node, error) {
	cert, _ := ctx.Value(peerCertKey).(*x509.Certificate)
	if cert == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("client certificate required"))
	}
	var n store.Node
	if err := a.s.st.DB.First(&n, "id = ?", cert.Subject.CommonName).Error; err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unknown node"))
	}
	if n.Revoked || n.CertSerial == "" || n.CertSerial != cert.SerialNumber.Text(16) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("certificate revoked"))
	}
	return &n, nil
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// Enroll exchanges a one-time install token for a client certificate.
func (a *agentService) Enroll(ctx context.Context, req *connect.Request[polarisv1.EnrollRequest]) (*connect.Response[polarisv1.EnrollResponse], error) {
	tok := strings.TrimSpace(req.Msg.Token)
	if tok == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("token required"))
	}
	var n store.Node
	if err := a.s.st.DB.First(&n, "token_hash = ?", hashToken(tok)).Error; err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("invalid token"))
	}
	if n.TokenExpires == nil || time.Now().After(*n.TokenExpires) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("token expired"))
	}
	// Burn the token atomically so two concurrent enrolments cannot both win.
	if res := a.s.st.DB.Model(&store.Node{}).Where("id = ? AND token_hash = ?", n.ID, hashToken(tok)).
		Update("token_hash", ""); res.Error != nil || res.RowsAffected != 1 {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("invalid token"))
	}
	certPEM, serial, err := a.s.ca.SignCSR(req.Msg.CsrPem, n.ID, 10*365*24*time.Hour)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	sys := n.System.V
	sys.Hostname = req.Msg.Hostname
	err = a.s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(map[string]any{
		"cert_serial": serial, "token_hash": "", "token_expires": nil, "revoked": false,
		"agent_version": req.Msg.AgentVersion, "status": "offline", "system": store.NewJSON(sys),
	}).Error
	if err != nil {
		return nil, err
	}
	a.s.audit("system", "node.enroll", n.Name, req.Msg.Hostname, req.Peer().Addr)
	if n.CertSerial == "" {
		a.s.applyBoundTemplates(n.ID) // first enrolment: apply label-bound templates
		go a.s.autoBindAfterEnroll(n.ID)
	}
	a.s.MarkDirty("enrolled", n.ID)
	return connect.NewResponse(&polarisv1.EnrollResponse{NodeId: n.ID, CertPem: certPEM, CaPem: a.s.ca.CertPEM}), nil
}

// Connect runs the bidirectional stream.
func (a *agentService) Connect(ctx context.Context, stream *connect.BidiStream[polarisv1.AgentMessage, polarisv1.PanelMessage]) error {
	n, err := a.node(ctx)
	if err != nil {
		return err
	}
	conn := a.s.hub.register(n.ID)
	defer func() {
		a.s.hub.unregister(conn)
		a.s.hbMu.Lock()
		delete(a.s.hbCores, n.ID)
		delete(a.s.progress, n.ID)
		delete(a.s.limited, n.ID)
		a.s.hbMu.Unlock()
		if !a.s.hub.Online(n.ID) {
			a.s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Update("status", "offline")
			a.s.broadcastNodes()
		}
	}()
	a.s.log.Info("agent connected", "node", n.Name, "peer", stream.Peer().Addr)

	if n.DesiredRev == 0 {
		_, _, _ = a.s.Publish(n.ID, "initial", true)
		a.s.st.DB.First(n, "id = ?", n.ID)
	}
	conn.out <- &polarisv1.PanelMessage{Msg: &polarisv1.PanelMessage_SpecChanged{SpecChanged: &polarisv1.SpecChanged{Rev: n.DesiredRev}}}

	errc := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				errc <- err
				return
			}
			a.handle(n.ID, msg)
		}
	}()
	for {
		select {
		case m := <-conn.out:
			if err := stream.Send(m); err != nil {
				return err
			}
		case <-conn.done:
			return nil
		case err := <-errc:
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func (a *agentService) handle(nodeID string, msg *polarisv1.AgentMessage) {
	switch m := msg.Msg.(type) {
	case *polarisv1.AgentMessage_Heartbeat:
		a.s.handleHeartbeat(nodeID, m.Heartbeat)
	case *polarisv1.AgentMessage_ApplyResult:
		a.s.handleApplyResult(nodeID, m.ApplyResult)
	case *polarisv1.AgentMessage_CommandResult:
		a.s.hub.resolve(nodeID, m.CommandResult)
	case *polarisv1.AgentMessage_OnlineIps:
		a.s.handleOnlineIPs(nodeID, m.OnlineIps)
	case *polarisv1.AgentMessage_Progress:
		a.s.handleProgress(nodeID, m.Progress)
	}
}

// GetSpec returns a revision (0 = latest desired).
func (a *agentService) GetSpec(ctx context.Context, req *connect.Request[polarisv1.GetSpecRequest]) (*connect.Response[polarisv1.GetSpecResponse], error) {
	n, err := a.node(ctx)
	if err != nil {
		return nil, err
	}
	rev := req.Msg.Rev
	if rev == 0 {
		rev = n.DesiredRev
	}
	if rev == 0 {
		if rev, _, err = a.s.Publish(n.ID, "initial", true); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	var r store.ConfigRevision
	if err := a.s.st.DB.Where("node_id = ? AND rev = ?", n.ID, rev).First(&r).Error; err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("revision %d not found", rev))
	}
	raw, err := a.s.st.Box.Open(r.Spec)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&polarisv1.GetSpecResponse{Rev: rev, SpecJson: []byte(raw)}), nil
}

// ReportTraffic buffers traffic batches; replays of a batch are ignored.
func (a *agentService) ReportTraffic(ctx context.Context, req *connect.Request[polarisv1.ReportTrafficRequest]) (*connect.Response[polarisv1.ReportTrafficResponse], error) {
	n, err := a.node(ctx)
	if err != nil {
		return nil, err
	}
	resp := &polarisv1.ReportTrafficResponse{}
	allowed := a.s.nodeUsers(n.ID)
	for _, b := range req.Msg.Batches {
		if b.BatchId == "" || a.s.traffic.Seen(n.ID+"/"+b.BatchId) {
			resp.AcceptedBatchIds = append(resp.AcceptedBatchIds, b.BatchId)
			continue
		}
		ds := make([]model.TrafficDelta, 0, len(b.Users))
		for _, u := range b.Users {
			if !allowed[u.UserId] && !isTagCounter(u.UserId) {
				continue // not a user of this node
			}
			ds = append(ds, model.TrafficDelta{UserID: u.UserId, Up: max(u.Up, 0), Down: max(u.Down, 0)})
		}
		if err := a.s.traffic.Add(n.ID, ds); err != nil {
			a.s.traffic.Forget(n.ID + "/" + b.BatchId) // let the retry count
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		resp.AcceptedBatchIds = append(resp.AcceptedBatchIds, b.BatchId)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) handleHeartbeat(nodeID string, hb *polarisv1.Heartbeat) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", nodeID).Error; err != nil {
		return
	}
	now := time.Now()
	status := "online"
	if hb.AppliedRev < n.DesiredRev {
		status = "syncing"
	}
	if n.LastError != "" && hb.AppliedRev < n.DesiredRev {
		status = "error"
	}
	s.observePublicIP(&n, hb.PublicIpv4, hb.PublicIpv6)
	s.hbMu.Lock()
	if s.limited == nil {
		s.limited = map[string][]*polarisv1.LimitedUser{}
	}
	s.limited[nodeID] = hb.Limited
	s.hbMu.Unlock()
	if n.Status == "offline" || n.Status == "pending" {
		if n.Status == "offline" && n.LastSeen != nil {
			s.notify(Event{Kind: "node_online", NodeID: n.ID, Text: "🟢 节点 " + n.Name + " 已恢复在线"})
		}
	}
	sys := n.System.V
	if hb.System != nil {
		sys.CPU, sys.Mem, sys.RxBps, sys.TxBps = hb.System.CpuPercent, hb.System.MemPercent, hb.System.RxBps, hb.System.TxBps
		sys.Uptime, sys.Load1 = hb.System.UptimeSeconds, hb.System.Load1
		s.recordMetric(nodeID, sys, now)
	}
	upd := map[string]any{
		"status": status, "last_seen": now, "applied_rev": hb.AppliedRev, "agent_version": hb.AgentVersion, "arch": hb.Arch,
		"system": store.NewJSON(sys),
	}
	if hb.System != nil {
		for k, v := range s.accountNodeNet(&n, hb.System.RxBytes, hb.System.TxBytes, now) {
			upd[k] = v
		}
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", nodeID).Updates(upd)
	// Core state rarely changes; with heartbeats every few seconds only write
	// it when it does (or once a minute).
	var kb strings.Builder
	for _, c := range hb.Cores {
		fmt.Fprintf(&kb, "%s|%s|%t|%d|%d|%s|%v|%t;", c.Type, c.Version, c.Running, c.Pid, c.StartedAt, c.Error, c.InstalledVersions, c.NoUserTraffic)
	}
	key := kb.String()
	s.hbMu.Lock()
	last := s.hbCores[nodeID]
	fresh := last.key == key && now.Sub(last.at) < time.Minute
	if !fresh {
		if s.hbCores == nil {
			s.hbCores = map[string]hbCoreState{}
		}
		s.hbCores[nodeID] = hbCoreState{key: key, at: now}
	}
	s.hbMu.Unlock()
	for _, c := range hb.Cores {
		if fresh {
			break
		}
		st := "stopped"
		if c.Running {
			st = "running"
		}
		upd := map[string]any{"status": st, "pid": int(c.Pid), "error": c.Error,
			"installed_versions": store.NewJSON(c.InstalledVersions), "running_version": c.Version, "no_user_traffic": c.NoUserTraffic}
		if c.StartedAt > 0 {
			t := time.Unix(c.StartedAt, 0)
			upd["started_at"] = &t
		}
		if err := s.st.DB.Model(&store.Core{}).Where("node_id = ? AND type = ?", nodeID, c.Type).Updates(upd).Error; err != nil {
			s.log.Error("update core state", "node", nodeID, "core", c.Type, "err", err)
		}
	}
	s.broadcastNodes()
}

func (s *Server) handleApplyResult(nodeID string, r *polarisv1.ApplyResult) {
	status := "applied"
	if !r.Ok {
		status = "failed"
	}
	s.st.DB.Model(&store.ConfigRevision{}).Where("node_id = ? AND rev = ?", nodeID, r.Rev).
		Updates(map[string]any{"status": status, "method": r.Method, "error": truncate(r.Error, 1900)})
	upd := map[string]any{"last_error": truncate(r.Error, 1900)}
	if r.Ok {
		upd["applied_rev"] = r.Rev
		upd["status"] = "online"
	} else {
		upd["status"] = "error"
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", nodeID).Updates(upd)
	if !r.Ok {
		s.revertFailedUpgrades(nodeID, r.Error)
		var n store.Node
		s.st.DB.Select("name").First(&n, "id = ?", nodeID)
		s.audit("agent", "spec.apply_failed", n.Name, "rev "+strconv.FormatInt(r.Rev, 10)+": "+r.Error, "")
		s.notify(Event{Kind: "apply_failed", NodeID: nodeID, Text: "⚠️ 节点 " + n.Name + " 应用 rev " + strconv.FormatInt(r.Rev, 10) + " 失败：" + truncate(r.Error, 300)})
	}
	s.broadcastNodes()
}

type hbCoreState struct {
	key string
	at  time.Time
}

// nodeProgress is what a node's agent is doing right now (kept in memory
// only). A finished activity stays visible for progressLinger so the admin
// sees how it ended.
type nodeProgress struct {
	Active    bool      `json:"active"`
	Stage     string    `json:"stage"`
	Done      int64     `json:"done"`
	Total     int64     `json:"total"`
	Rev       int64     `json:"rev,omitempty"`
	StartedAt int64     `json:"startedAt"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
}

const progressLinger = time.Minute

func (s *Server) handleProgress(nodeID string, p *polarisv1.Progress) {
	s.hbMu.Lock()
	if s.progress == nil {
		s.progress = map[string]*nodeProgress{}
	}
	s.progress[nodeID] = &nodeProgress{Active: p.Active, Stage: truncate(p.Stage, 200), Done: p.Done, Total: p.Total,
		Rev: p.Rev, StartedAt: p.StartedAt, Error: truncate(p.Error, 500), At: time.Now()}
	s.hbMu.Unlock()
	s.broadcastNodes()
}

// nodeProgressFor returns the activity to show for a node, if any.
func (s *Server) nodeProgressFor(nodeID string) *nodeProgress {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	p := s.progress[nodeID]
	if p == nil {
		return nil
	}
	linger := progressLinger
	if p.Error != "" {
		linger = 10 * time.Minute // a failure stays until someone has seen it
	}
	if !p.Active && time.Since(p.At) > linger {
		delete(s.progress, nodeID)
		return nil
	}
	c := *p
	return &c
}
