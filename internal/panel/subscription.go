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
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

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
	title := s.st.GetSetting("site_title", "Polaris")
	info := sub.Info{Title: title, Username: u.Username, Upload: u.UsedUp, Download: u.UsedDown, Total: u.TrafficLimit,
		Expire: u.ExpireAt, URL: s.subURL(u.SubToken)}
	var nodes []sub.Node
	if u.Status == "active" {
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
	s.serveNodes(c, nodes, info, nil, s.converterFor(nil), s.subURL(u.SubToken)+"?raw=1", &u)
}

// subNodes expands every authorised inbound × endpoint into client nodes.
func (s *Server) subNodes(u *store.User) ([]sub.Node, error) {
	var ins []store.Inbound
	err := s.st.DB.Table("inbounds").Select("DISTINCT inbounds.*").
		Joins("JOIN group_inbounds ON group_inbounds.inbound_id = inbounds.id").
		Joins("JOIN user_groups ON user_groups.group_id = group_inbounds.group_id").
		Where("user_groups.user_id = ? AND inbounds.enabled = ? AND inbounds.service_only = ?", u.ID, true, false).
		Order("inbounds.node_id, inbounds.port").Find(&ins).Error
	if err != nil {
		return nil, err
	}
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
	nameTpl := s.st.GetSetting("sub_name_template", "{flag} {node} {protocol}")
	var out []sub.Node
	for _, in := range ins {
		node, ok := nodes[in.NodeID]
		if !ok {
			continue
		}
		m := s.toModelInbound(&in)
		base := s.clientNode(&m, node, u)
		base.Region, base.Source = node.Flag, "panel"
		vars := map[string]string{"flag": sub.Flag(node.Flag), "region": node.Region, "node": node.Name,
			"protocol": sub.ProtocolLabel(base), "tag": in.Tag, "kind": ""}
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

// clientNode builds the client view of an inbound for a user (direct entry).
func (s *Server) clientNode(m *model.Inbound, node *store.Node, u *store.User) sub.Node {
	um := u.ToModel()
	n := sub.Node{Server: nodeHost(node), Port: m.Port, Protocol: m.Protocol.Type, UUID: u.UUID, Password: u.Password,
		Username: u.ID, Flow: m.Protocol.Flow, Transport: m.Transport,
		UpMbps: m.Protocol.UpMbps, DownMbps: m.Protocol.DownMbps, Obfs: m.Protocol.Obfs, ObfsPassword: m.Protocol.ObfsPassword,
		CongestionControl: m.Protocol.CongestionControl}
	if m.Protocol.Type == model.ProtoShadowsocks {
		n.Method = m.Protocol.Method
		n.Password = um.SSKey(m.Protocol.Method)
		if model.SS2022KeyLen(m.Protocol.Method) > 0 {
			n.Password = m.Protocol.ServerKey + ":" + n.Password
		}
	}
	switch m.Security.Type {
	case model.SecTLS:
		n.TLS = model.ClientTLS{Type: model.SecTLS, ServerName: orDefault(m.Security.ServerName, node.Domain), ALPN: m.Security.ALPN,
			Fingerprint: m.Security.Fingerprint, Insecure: s.tlsInsecure(m.Security.CertificateID)}
	case model.SecReality:
		n.TLS = model.ClientTLS{Type: model.SecReality, ServerName: first(m.Security.RealityServerNames),
			PublicKey: m.Security.RealityPublicKey, ShortID: first(m.Security.RealityShortIDs), Fingerprint: orDefault(m.Security.Fingerprint, "chrome")}
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
	base := s.cfg.PublicURL
	if base == "" {
		base = "http://" + c.Request.Host
	}
	script := strings.NewReplacer("__PANEL_HTTP__", base, "__PANEL_VERSION__", Version,
		"__SHA_AMD64__", s.agentSHA("amd64"), "__SHA_ARM64__", s.agentSHA("arm64")).Replace(installSh)
	c.Data(http.StatusOK, "text/x-shellscript; charset=utf-8", []byte(script))
}

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
