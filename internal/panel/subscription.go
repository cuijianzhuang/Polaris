package panel

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

//go:embed assets/install.sh
var installSh string

func shareLink(n sub.Node) string { return sub.ShareLink(n) }

// subscription serves /sub/{token}, choosing the renderer from ?format= or UA.
func (s *Server) subscription(c *gin.Context) {
	if !s.subEnabled() {
		c.String(http.StatusNotFound, "404 page not found")
		return
	}
	if !s.subLimit.Allow(clientIP(c.Request)) {
		c.String(http.StatusTooManyRequests, "too many requests")
		return
	}
	tok := c.Param("token")
	if len(tok) < 16 {
		c.String(http.StatusNotFound, "404 page not found")
		return
	}
	var u store.User
	if err := s.st.DB.First(&u, "sub_token = ? AND is_service = ?", tok, false).Error; err != nil {
		c.String(http.StatusNotFound, "404 page not found")
		return
	}
	deviceOK := s.checkDevice(&u, c.Request)
	title := s.st.GetSetting("site_title", "Polaris")
	info := sub.Info{Title: title, Username: u.Username, Upload: u.UsedUp, Download: u.UsedDown, Total: u.TrafficLimit,
		Expire: u.ExpireAt, URL: s.subURL(u.SubToken)}
	var nodes []sub.Node
	if s.servedUser(&u) && deviceOK {
		var err error
		if nodes, err = s.subNodes(&u); err != nil {
			serverError(c, err)
			return
		}
		if s.st.GetSetting("sub_apply_ops_users", "false") == "true" {
			if out, err := sub.ApplyChain(nodes, s.globalOperators()); err == nil {
				nodes = out
			}
		}
	}
	if len(nodes) == 0 && c.Query("raw") != "1" && sub.DetectFormat(c.Query("format"), c.GetHeader("User-Agent")) != "html" {
		// Tell the user why instead of handing clients an empty list.
		reason := s.emptyReason(&u)
		if !deviceOK {
			reason = fmt.Sprintf("设备数已达上限（%d 台），请在面板移除旧设备", u.DeviceLimit)
		}
		nodes = []sub.Node{reasonNode(reason)}
	}
	s.serveNodes(c, nodes, info, nil, s.converterFor(nil), s.renderOptsFor(nil), "/sub/"+u.SubToken, &u)
}

// subNodes expands every authorised inbound × endpoint into client nodes.
func (s *Server) subNodes(u *store.User) ([]sub.Node, error) {
	var ins []store.Inbound
	err := s.st.DB.Table("inbounds").Select("DISTINCT inbounds.*").
		Joins("JOIN ("+store.GrantsSQL+") grants ON grants.inbound_id = inbounds.id").
		Where("grants.user_id = ? AND inbounds.enabled = ? AND inbounds.service_only = ?", u.ID, true, false).
		Order("inbounds.node_id, inbounds.port").Find(&ins).Error
	if err != nil {
		return nil, err
	}
	// Per-node quotas: no node past its cap; out of account quota, only the
	// nodes with a traffic pack left.
	capped, pack := s.nodeAccess(u.ID)
	kept := ins[:0]
	for _, in := range ins {
		if !capped[in.NodeID] && (u.Status == "active" || pack[in.NodeID]) {
			kept = append(kept, in)
		}
	}
	ins = kept
	nodes := map[string]*store.Node{}
	var ns []store.Node
	s.st.DB.Where("revoked = ?", false).Order("sort_order, name").Find(&ns)
	order := map[string]int{}
	for i := range ns {
		nodes[ns[i].ID] = &ns[i]
		order[ns[i].ID] = i
	}
	var chains []store.Chain
	s.st.DB.Find(&chains)
	nodeIDs := make([]string, 0, len(ins))
	for _, in := range ins {
		nodeIDs = append(nodeIDs, in.NodeID)
	}
	masters := s.fallbackMasters(nodeIDs)
	var xrayCores []store.Core
	s.st.DB.Where("node_id IN ? AND type = ?", nodeIDs, model.CoreXray).Find(&xrayCores)
	xrayVer := map[string]string{}
	for _, c := range xrayCores {
		xrayVer[c.NodeID] = orDefault(c.RunningVersion, c.Version)
	}
	nameTpl := s.st.GetSetting("sub_name_template", "{flag} {node} {protocol}")
	showRate := s.st.GetSetting("sub_show_rate", "true") == "true"
	var out []sub.Node
	for _, in := range ins {
		node, ok := nodes[in.NodeID]
		if !ok {
			continue
		}
		m := s.toModelInbound(&in)
		if master := masters[in.ID]; master != nil {
			// Fallback child: reachable only through the master's port and TLS.
			mm := s.toModelInbound(master)
			m.Port, m.Security = mm.Port, mm.Security
		}
		base := s.clientNode(&m, node, u)
		base.Region, base.Source = node.Flag, "panel"
		if in.Core == model.CoreXray && base.TLS.Type == model.SecReality && core.VersionAtLeast(xrayVer[node.ID], model.XrayPQRealityVersion) {
			base.PQRealityOnly = true
		}
		vars := map[string]string{"flag": sub.Flag(node.Flag), "region": node.Region, "node": node.Name,
			"protocol": sub.ProtocolLabel(base), "tag": in.Tag, "kind": "", "rate": ""}
		if showRate {
			vars["rate"] = rateLabel(node.TrafficRate)
		}
		// Proxy-chain relay inbounds are named "[relay] → [landing]".
		for _, ch := range chains {
			hops := ch.Hops.V
			if ch.Mode == "proxy" && len(hops) >= 2 && hops[0].InboundID == in.ID {
				if land, ok := nodes[hops[len(hops)-1].NodeID]; ok {
					vars["node"] = node.Name + " → " + land.Name
				}
			}
		}
		var eps []store.Endpoint
		s.st.DB.Where("inbound_id = ?", in.ID).Order("sort_order, created_at").Find(&eps)
		if len(eps) == 0 {
			eps = []store.Endpoint{{Kind: "direct"}}
		}
		for _, ep := range eps {
			out = append(out, s.expandEndpoint(base, ep, vars, nameTpl, nodes, chains, &m)...)
		}
	}
	return sub.Dedupe(out), nil
}

// rateLabel shows a node's traffic multiplier in node names ("×2"); empty
// at the normal rate.
func rateLabel(rate float64) string {
	if rate <= 0 || rate == 1 {
		return ""
	}
	return "×" + strconv.FormatFloat(rate, 'f', -1, 64)
}

// clientNode builds the client view of an inbound for a user (direct entry).
func (s *Server) clientNode(m *model.Inbound, node *store.Node, u *store.User) sub.Node {
	um := u.ToModel()
	n := sub.Node{Server: nodeHost(node), Port: m.Port, Protocol: m.Protocol.Type, UUID: u.UUID, Password: u.Password,
		Username: u.ID, Flow: m.Protocol.Flow, Transport: m.Transport,
		UpMbps: m.Protocol.UpMbps, DownMbps: m.Protocol.DownMbps, Obfs: m.Protocol.Obfs, ObfsPassword: m.Protocol.ObfsPassword,
		CongestionControl: m.Protocol.CongestionControl}
	if m.Protocol.Obfs == "" {
		n.ObfsPassword = "" // the server has obfs off
	}
	n.Encryption = m.Protocol.Encryption
	if m.Protocol.Type == model.ProtoShadowsocks {
		n.Method = m.Protocol.Method
		n.Password = ssClientPassword(m, &um)
	}
	switch m.Security.Type {
	case model.SecTLS:
		n.TLS = model.ClientTLS{Type: model.SecTLS, ServerName: orDefault(m.Security.ServerName, node.Domain), ALPN: m.Security.ALPN,
			Fingerprint: m.Security.Fingerprint, Insecure: s.tlsInsecure(m.Security.CertificateID)}
	case model.SecReality:
		n.TLS = model.ClientTLS{Type: model.SecReality, ServerName: first(m.Security.RealityServerNames),
			PublicKey: m.Security.RealityPublicKey, ShortID: first(m.Security.RealityShortIDs), Fingerprint: orDefault(m.Security.Fingerprint, "chrome"),
			SpiderX: m.Security.RealitySpiderX, Mldsa65Verify: m.Security.RealityMldsa65Verify}
	}
	return n
}

func (s *Server) expandEndpoint(base sub.Node, ep store.Endpoint, vars map[string]string, nameTpl string,
	nodes map[string]*store.Node, chains []store.Chain, m *model.Inbound) []sub.Node {
	name := func(kind string) string {
		v := map[string]string{}
		for k, x := range vars {
			v[k] = x
		}
		v["kind"] = kind
		n := sub.NameVars(nameTpl, v)
		if kind != "" && !strings.Contains(nameTpl, "{kind}") {
			n += " " + kind
		}
		if r := v["rate"]; r != "" && !strings.Contains(nameTpl, "{rate}") {
			n += " " + r
		}
		if ep.Remark != "" {
			n += " " + ep.Remark
		}
		return n
	}
	apply := func(n sub.Node) sub.Node {
		if ep.Address != "" {
			n.Server = ep.Address
		}
		if ep.Port > 0 {
			n.Port = ep.Port
		}
		if ep.SNI != "" {
			n.TLS.ServerName = ep.SNI
		}
		if ep.Host != "" {
			n.Transport.Host = ep.Host
		}
		if ep.Path != "" {
			n.Transport.Path = ep.Path
		}
		return n
	}
	switch ep.Kind {
	case "cdn":
		var p store.CDNProfile
		if err := s.st.DB.First(&p, "id = ?", ep.CDNProfileID).Error; err != nil || !m.CDNCapable() {
			return nil
		}
		n := base
		n.Server = p.OriginDomain
		n.Port = 443
		if len(p.Ports.V) > 0 {
			n.Port = p.Ports.V[0]
		}
		if n.TLS.Type == model.SecNone || n.TLS.Type == "" {
			n.TLS = model.ClientTLS{Type: model.SecTLS}
		}
		n.TLS.ServerName = p.OriginDomain
		n.TLS.Insecure = false
		n.Transport.Host = p.OriginDomain
		n = apply(n)
		if !p.ExpandPerIP || len(p.PreferredIPs.V) == 0 {
			n.Name = name("CDN")
			return []sub.Node{n}
		}
		ips := p.PreferredIPs.V
		if p.IPTopN > 0 && len(ips) > p.IPTopN {
			ips = ips[:p.IPTopN]
		}
		var out []sub.Node
		for i, ip := range ips {
			x := n
			x.Server = ip
			x.Name = name(fmt.Sprintf("CDN-%d", i+1))
			out = append(out, x)
		}
		return out
	case "relay":
		for _, ch := range chains {
			if ch.ID != ep.RelayChainID || ch.Mode != "l4" || len(ch.Hops.V) < 2 {
				continue
			}
			entry, ok := nodes[ch.Hops.V[0].NodeID]
			if !ok {
				return nil
			}
			n := base
			n.Server, n.Port = nodeHost(entry), ch.Hops.V[0].Port
			n = apply(n)
			v := map[string]string{}
			for k, x := range vars {
				v[k] = x
			}
			v["node"] = entry.Name + " → " + vars["node"]
			v["flag"] = sub.Flag(entry.Flag)
			n.Name = sub.NameVars(nameTpl, v)
			return []sub.Node{n}
		}
		return nil
	default:
		n := base
		if fi, ok := s.frontFor(m.ID); ok {
			// Fronted inbounds are only reachable through Caddy's public port.
			n.Port = fi.Port
			n.TLS.ServerName = fi.Domain
			n.Transport.Host = fi.Domain
		}
		n = apply(n)
		n.Name = name("")
		return []sub.Node{n}
	}
}

// installScript serves the agent installer.
func (s *Server) installScript(c *gin.Context) {
	// The Host header ends up inside a shell script: only allow host[:port].
	if s.cfg.PublicURL == "" && !validHost.MatchString(c.Request.Host) {
		c.String(http.StatusBadRequest, "invalid Host header; set POLARIS_PUBLIC_URL")
		return
	}
	c.Data(http.StatusOK, "text/x-shellscript; charset=utf-8", []byte(s.renderInstallScript(c.Request, "")))
}

var joinTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{12,64}$`)

// joinScript serves install.sh with a one-time token baked in (GET /j/:token).
// Unknown or expired tokens get a script that explains instead of installing.
func (s *Server) joinScript(c *gin.Context) {
	tok := c.Param("token")
	fail := func(msg string) {
		c.Data(http.StatusNotFound, "text/x-shellscript; charset=utf-8", []byte("#!/usr/bin/env bash\necho '✗ "+msg+"' >&2\nexit 1\n"))
	}
	if !s.subLimit.Allow(clientIP(c.Request)) {
		fail("请求过于频繁，请稍后再试")
		return
	}
	var n store.Node
	if !joinTokenRe.MatchString(tok) || s.st.DB.First(&n, "token_hash = ?", hashToken(tok)).Error != nil ||
		n.TokenExpires == nil || time.Now().After(*n.TokenExpires) {
		fail("安装链接无效或已过期（24 小时有效、只能使用一次），请在面板「节点」中重新生成")
		return
	}
	if s.cfg.PublicURL == "" && !validHost.MatchString(c.Request.Host) {
		fail("invalid Host header; set POLARIS_PUBLIC_URL")
		return
	}
	c.Data(http.StatusOK, "text/x-shellscript; charset=utf-8", []byte(s.renderInstallScript(c.Request, tok)))
}

func (s *Server) renderInstallScript(r *http.Request, tok string) string {
	return strings.NewReplacer("__PANEL_HTTP__", s.requestBase(r), "__PANEL_VERSION__", Version, "__PANEL_TOKEN__", tok,
		"__PANEL_AGENT__", s.agentURLFor(r), "__PANEL_CA__", s.ca.Fingerprint(),
		"__SHA_AMD64__", s.agentSHA("amd64"), "__SHA_ARM64__", s.agentSHA("arm64"),
		"__AGENT_ASSET__", "polaris-agent-"+editionAsset()+"linux").Replace(installSh)
}

var validHost = regexp.MustCompile(`^(\[[0-9a-fA-F:.]+\]|[A-Za-z0-9.-]+)(:[0-9]{1,5})?$`)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

type shaEntry struct {
	mod  time.Time
	size int64
	sum  string
}

var agentSHACache sync.Map // path -> shaEntry

// agentSHA returns the SHA-256 of a locally served agent binary ("" if absent).
func (s *Server) agentSHA(arch string) string {
	p := filepath.Join(s.cfg.DataDir, "downloads", "polaris-agent-linux-"+arch)
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	if v, ok := agentSHACache.Load(p); ok {
		if e := v.(shaEntry); e.mod.Equal(fi.ModTime()) && e.size == fi.Size() {
			return e.sum
		}
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	sum := hex.EncodeToString(h.Sum(nil))
	agentSHACache.Store(p, shaEntry{fi.ModTime(), fi.Size(), sum})
	return sum
}
