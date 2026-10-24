package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// toModelInbound converts a stored inbound, decrypting secrets. Certificates
// are resolved separately by the spec builder.
func (s *Server) toModelInbound(in *store.Inbound) model.Inbound {
	m := model.Inbound{
		ID: in.ID, NodeID: in.NodeID, Tag: in.Tag, Core: in.Core, Listen: in.Listen, Port: in.Port,
		Protocol: in.Protocol.V, Transport: in.Transport.V, Security: in.Security.V, Sniffing: in.Sniffing.V,
		Extra: in.Extra, Enabled: in.Enabled, OutboundIPStrategy: in.OutboundIPStrategy,
	}
	m.Security.RealityPrivateKey = s.st.Box.MustOpen(m.Security.RealityPrivateKey)
	m.Protocol.ServerKey = s.st.Box.MustOpen(m.Protocol.ServerKey)
	m.Protocol.Decryption = s.st.Box.MustOpen(m.Protocol.Decryption)
	m.Security.RealityMldsa65Seed = s.st.Box.MustOpen(m.Security.RealityMldsa65Seed)
	return m
}

// keepSecrets carries the old inbound's write-only secrets into an edit that
// left them blank (the API never returns them): the VLESS decryption key
// while the client key is unchanged, and the ML-DSA-65 seed while its
// verify key is unchanged.
func keepSecrets(m, old *model.Inbound) {
	if m.Protocol.Type == model.ProtoSnell && old.Protocol.Type == model.ProtoSnell && m.Protocol.ServerKey == "" {
		m.Protocol.ServerKey = old.Protocol.ServerKey
	}
	if m.Protocol.Decryption == "" && m.Protocol.Encryption != "" && m.Protocol.Encryption == old.Protocol.Encryption {
		m.Protocol.Decryption = old.Protocol.Decryption
	}
	if m.Security.RealityMldsa65Seed == "" && m.Security.RealityMldsa65Verify != "" &&
		m.Security.RealityMldsa65Verify == old.Security.RealityMldsa65Verify {
		m.Security.RealityMldsa65Seed = old.Security.RealityMldsa65Seed
	}
}

// sealInbound encrypts secret fields before persisting.
func (s *Server) sealInbound(in *store.Inbound) {
	in.Security.V.RealityPrivateKey = s.st.Box.Seal(in.Security.V.RealityPrivateKey)
	in.Protocol.V.ServerKey = s.st.Box.Seal(in.Protocol.V.ServerKey)
	in.Protocol.V.Decryption = s.st.Box.Seal(in.Protocol.V.Decryption)
	in.Security.V.RealityMldsa65Seed = s.st.Box.Seal(in.Security.V.RealityMldsa65Seed)
	// Never persist resolved PEM inside the inbound row.
	in.Security.V.CertPEM, in.Security.V.KeyPEM = "", ""
}

// prepareInbound fills generated secrets (REALITY keys, SS server PSK, random
// paths) and normalises the inbound. It is idempotent.
func (s *Server) prepareInbound(m *model.Inbound) error {
	m.Normalize()
	if m.Security.Type == model.SecReality {
		if m.Security.RealityPrivateKey == "" {
			priv, pub, err := pki.RealityKeyPair()
			if err != nil {
				return err
			}
			m.Security.RealityPrivateKey, m.Security.RealityPublicKey = priv, pub
		} else if m.Security.RealityPublicKey == "" {
			pub, err := pki.RealityPublicKey(m.Security.RealityPrivateKey)
			if err != nil {
				return fmt.Errorf("reality private key: %w", err)
			}
			m.Security.RealityPublicKey = pub
		}
		if len(m.Security.RealityShortIDs) == 0 {
			m.Security.RealityShortIDs = []string{pki.ShortID(8)}
		}
		if len(m.Security.RealityServerNames) == 0 && m.Security.RealityDest != "" {
			host := m.Security.RealityDest
			if i := strings.LastIndex(host, ":"); i > 0 {
				host = host[:i]
			}
			m.Security.RealityServerNames = []string{host}
		}
	}
	if m.Protocol.Type == model.ProtoTUIC && m.Security.Type == model.SecTLS && len(m.Security.ALPN) == 0 {
		m.Security.ALPN = []string{"h3"} // clients offer h3; a server without ALPN rejects the handshake
	}
	if m.Protocol.Type == model.ProtoSnell && m.Protocol.ServerKey == "" {
		m.Protocol.ServerKey = pki.Password(32)
	}
	if m.Protocol.Type == model.ProtoShadowsocks {
		if m.Protocol.Method == "" {
			m.Protocol.Method = "2022-blake3-aes-128-gcm"
		}
		if model.SS2022KeyLen(m.Protocol.Method) > 0 && m.Protocol.ServerKey == "" {
			k, err := pki.SS2022Key(m.Protocol.Method)
			if err != nil {
				return err
			}
			m.Protocol.ServerKey = k
		} else if m.Protocol.ServerKey == "" {
			// Legacy ciphers: only mihomo's single-user listener uses it.
			m.Protocol.ServerKey = pki.Password(16)
		}
	}
	switch m.Transport.Type {
	case model.TransWS, model.TransHTTPUpgrade, model.TransXHTTP:
		if m.Transport.Path == "" {
			m.Transport.Path = pki.RandomPath()
		}
	case model.TransGRPC:
		if m.Transport.ServiceName == "" {
			m.Transport.ServiceName = pki.Hex(4)
		}
	}
	if m.Protocol.Type == model.ProtoHysteria2 && m.Protocol.Obfs == "salamander" && m.Protocol.ObfsPassword == "" {
		m.Protocol.ObfsPassword = pki.Password(16)
	}
	if err := prepareXrayExtras(m); err != nil {
		return err
	}
	if m.Sniffing.Enabled && len(m.Sniffing.DestOverride) == 0 {
		m.Sniffing.DestOverride = []string{"http", "tls", "quic"}
	}
	return m.Validate()
}

// nodeCores returns the cores installed on a node.
func (s *Server) nodeCores(nodeID string) ([]store.Core, error) {
	var cs []store.Core
	err := s.st.DB.Where("node_id = ?", nodeID).Order("is_default desc, type").Find(&cs).Error
	return cs, err
}

// assignCore implements the allocation strategy: an explicit core must support
// the combination; "auto" prefers the node default, then any capable core.
func (s *Server) assignCore(node *store.Node, m *model.Inbound) error {
	cores, err := s.nodeCores(node.ID)
	if err != nil {
		return err
	}
	if len(cores) == 0 {
		return fmt.Errorf("node %s has no core installed", node.Name)
	}
	check := func(c store.Core) error {
		if m.Protocol.Type == model.ProtoSnell {
			if !contains(node.AgentFeatures.V, model.SnellUsersFeature) {
				return fmt.Errorf("节点需要先升级 Agent 并完成配置拉取，才能使用 Snell 独立用户授权")
			}
			if c.NoUserTraffic {
				return fmt.Errorf("Snell 需要带 with_v2ray_api 的 sing-box，当前内核无法按用户计费")
			}
		}
		caps, ok := core.Capabilities(c.Type, c.Version)
		if !ok {
			return fmt.Errorf("unknown core %s", c.Type)
		}
		return caps.SupportsInbound(m)
	}
	if m.Core != "" && m.Core != model.CoreAuto {
		for _, c := range cores {
			if c.Type == m.Core {
				return check(c)
			}
		}
		return fmt.Errorf("core %s is not installed on %s", m.Core, node.Name)
	}
	var reasons []string
	ordered := cores
	for i, c := range cores {
		if c.Type == node.DefaultCore {
			ordered = append([]store.Core{c}, append(append([]store.Core{}, cores[:i]...), cores[i+1:]...)...)
			break
		}
	}
	for _, c := range ordered {
		if err := check(c); err == nil {
			m.Core = c.Type
			return nil
		} else {
			reasons = append(reasons, err.Error())
		}
	}
	return fmt.Errorf("no core on %s supports this inbound: %s", node.Name, strings.Join(reasons, "; "))
}

// portConflict reports another inbound or relay forward using the same port
// and L4 network on the node.
func (s *Server) portConflict(nodeID string, m *model.Inbound, excludeID string) error {
	return s.portConflictExcept(nodeID, m, excludeID, "")
}

// portConflictExcept is portConflict ignoring the hops of one chain (the
// chain being edited).
func (s *Server) portConflictExcept(nodeID string, m *model.Inbound, excludeID, excludeChain string) error {
	var ins []store.Inbound
	if err := s.st.DB.Where("node_id = ? AND port = ? AND id <> ?", nodeID, m.Port, excludeID).Find(&ins).Error; err != nil {
		return err
	}
	for _, o := range ins {
		om := s.toModelInbound(&o)
		om.Normalize()
		if (om.UsesTCP() && m.UsesTCP()) || (om.UsesUDP() && m.UsesUDP()) {
			return fmt.Errorf("port %d (%s) is already used by inbound %s", m.Port, m.Network(), o.Tag)
		}
	}
	var chains []store.Chain
	if err := s.st.DB.Where("mode = ?", "l4").Find(&chains).Error; err != nil {
		return err
	}
	for _, c := range chains {
		if c.ID == excludeChain {
			continue
		}
		for _, h := range c.Hops.V {
			if h.NodeID == nodeID && ((h.Port == m.Port && h.InboundID == "") || (h.TunnelPort > 0 && h.TunnelPort == m.Port)) {
				return fmt.Errorf("port %d is used by relay chain %s", m.Port, c.Name)
			}
		}
	}
	var fwds []store.ForwardRule
	s.st.DB.Where("node_id = ? AND listen_port = ? AND id <> ?", nodeID, m.Port, excludeID).Find(&fwds) // ids are unique across tables
	for _, f := range fwds {
		n := listenNet(f.Engine, f.TunnelRole, f.TunnelType, f.Network)
		if (m.UsesTCP() && n != "udp") || (m.UsesUDP() && n != "tcp") {
			return fmt.Errorf("port %d is used by forward rule %s", m.Port, f.Name)
		}
	}
	return nil
}

// usedPorts returns ports occupied on a node (both networks), for templates.
func (s *Server) usedPorts(nodeID string) (map[int]bool, error) {
	var ins []store.Inbound
	if err := s.st.DB.Where("node_id = ?", nodeID).Find(&ins).Error; err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, i := range ins {
		used[i.Port] = true
	}
	var chains []store.Chain
	if err := s.st.DB.Where("mode = ?", "l4").Find(&chains).Error; err != nil {
		return nil, err
	}
	for _, c := range chains {
		for _, h := range c.Hops.V {
			if h.NodeID == nodeID {
				if h.Port > 0 {
					used[h.Port] = true
				}
				if h.TunnelPort > 0 {
					used[h.TunnelPort] = true
				}
			}
		}
	}
	var fwds []store.ForwardRule
	s.st.DB.Where("node_id = ?", nodeID).Find(&fwds)
	for _, f := range fwds {
		used[f.ListenPort] = true
	}
	// Ports other programs hold on the node (nginx, sshd …), when it is online.
	for p := range s.nodeBusyPorts(nodeID) {
		used[p] = true
	}
	return used, nil
}

var busyCache struct {
	sync.Mutex
	m map[string]busyEntry
}

type busyEntry struct {
	at    time.Time
	ports map[int]bool
}

// nodeBusyPorts asks the agent which TCP / UDP ports are in use on the host
// (cached for 30 s; empty when the node is offline or the agent is too old).
func (s *Server) nodeBusyPorts(nodeID string) map[int]bool {
	busyCache.Lock()
	if e, ok := busyCache.m[nodeID]; ok && time.Since(e.at) < 30*time.Second {
		busyCache.Unlock()
		return e.ports
	}
	busyCache.Unlock()
	ports := map[int]bool{}
	if s.hub != nil && s.hub.Online(nodeID) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		res, err := s.hub.Command(ctx, nodeID, "busy_ports", nil, 8*time.Second)
		cancel()
		if err == nil && res.Ok {
			var v map[string][]int
			if json.Unmarshal([]byte(res.Output), &v) == nil {
				for _, l := range v {
					for _, p := range l {
						ports[p] = true
					}
				}
			}
		}
	}
	busyCache.Lock()
	if busyCache.m == nil {
		busyCache.m = map[string]busyEntry{}
	}
	busyCache.m[nodeID] = busyEntry{time.Now(), ports}
	busyCache.Unlock()
	return ports
}

// randomFreePort picks a random port in [lo, hi] that no inbound, chain or
// other program on the node uses.
func (s *Server) randomFreePort(nodeID string, lo, hi int) int {
	if lo <= 0 || hi < lo {
		lo, hi = 20000, 60000
	}
	used, _ := s.usedPorts(nodeID)
	for tries := 0; tries < 500; tries++ {
		if p := lo + rand.IntN(hi-lo+1); !used[p] {
			return p
		}
	}
	return 0
}

// ensureCert makes sure a TLS inbound references a certificate, issuing a
// self-signed one for its SNI when none is selected.
func (s *Server) ensureCert(node *store.Node, m *model.Inbound) error {
	if m.Security.Type != model.SecTLS || m.Security.CertificateID != "" {
		return nil
	}
	host := m.Security.ServerName
	if host == "" {
		host = node.Domain
	}
	if host == "" {
		host = node.Address
	}
	if host == "" {
		host = "polaris.local"
	}
	if m.Security.ServerName == "" {
		m.Security.ServerName = host
	}
	cert, err := s.findOrCreateSelfSigned(host)
	if err != nil {
		return err
	}
	m.Security.CertificateID = cert.ID
	return nil
}

// tlsInsecure reports whether clients must skip verification for the inbound.
func (s *Server) tlsInsecure(certID string) bool {
	if certID == "" {
		return false
	}
	var c store.Certificate
	if err := s.st.DB.Select("provider").First(&c, "id = ?", certID).Error; err != nil {
		return false
	}
	return c.Provider == "selfsigned"
}

// ssClientPassword is the password a client (or chain hop) uses for a
// shadowsocks inbound. mihomo's listener is single-user, so everyone uses
// the server key there; other cores take the 2022 "server:user" pair or the
// user's own legacy key.
func ssClientPassword(m *model.Inbound, u *model.User) string {
	if m.Core == model.CoreMihomo {
		return m.Protocol.ServerKey
	}
	pw := u.SSKey(m.Protocol.Method)
	if model.SS2022KeyLen(m.Protocol.Method) > 0 {
		pw = m.Protocol.ServerKey + ":" + pw
	}
	return pw
}

// prepareXrayExtras checks the newer Xray-only options and fills derivable
// values (the ML-DSA-65 verify key from its seed).
func prepareXrayExtras(m *model.Inbound) error {
	p := &m.Protocol
	if p.Decryption == "none" {
		p.Decryption = ""
	}
	if p.Encryption == "none" {
		p.Encryption = ""
	}
	if p.Decryption != "" || p.Encryption != "" {
		if p.Type != model.ProtoVLESS {
			return fmt.Errorf("VLESS Encryption 仅适用于 VLESS")
		}
		if p.Decryption == "" || p.Encryption == "" {
			return fmt.Errorf("VLESS Encryption 需要同时填写 decryption 与 encryption（可在表单中一键生成）")
		}
	}
	if n := len(p.TestSeed); n != 0 && n != 4 {
		return fmt.Errorf("Vision testseed 需要 4 个数字")
	}
	sec := &m.Security
	if sec.Type != model.SecReality {
		sec.RealityMldsa65Seed, sec.RealityMldsa65Verify, sec.RealitySpiderX = "", "", ""
		sec.RealityLimitUpload, sec.RealityLimitDownload = nil, nil
		sec.RealityMinClientVer, sec.RealityMaxClientVer, sec.RealityMaxTimeDiff = "", "", 0
		return nil
	}
	if sec.RealityMldsa65Seed != "" {
		if sec.RealityMldsa65Seed == sec.RealityPrivateKey {
			return fmt.Errorf("mldsa65 seed 不能与 REALITY 私钥相同")
		}
		v, err := pki.MLDSA65Verify(sec.RealityMldsa65Seed)
		if err != nil {
			return err
		}
		sec.RealityMldsa65Verify = v
	} else {
		sec.RealityMldsa65Verify = ""
	}
	for _, v := range []string{sec.RealityMinClientVer, sec.RealityMaxClientVer} {
		if v != "" && !clientVerRe.MatchString(v) {
			return fmt.Errorf("客户端版本应为 x.y.z（每段 0–255）：%s", v)
		}
	}
	return nil
}

var clientVerRe = regexp.MustCompile(`^(25[0-5]|2[0-4]\d|1?\d?\d)(\.(25[0-5]|2[0-4]\d|1?\d?\d)){0,2}$`)
