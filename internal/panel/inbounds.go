package panel

import (
	"fmt"
	"strings"

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
		Extra: in.Extra, Enabled: in.Enabled,
	}
	m.Security.RealityPrivateKey = s.st.Box.MustOpen(m.Security.RealityPrivateKey)
	m.Protocol.ServerKey = s.st.Box.MustOpen(m.Protocol.ServerKey)
	return m
}

// sealInbound encrypts secret fields before persisting.
func (s *Server) sealInbound(in *store.Inbound) {
	in.Security.V.RealityPrivateKey = s.st.Box.Seal(in.Security.V.RealityPrivateKey)
	in.Protocol.V.ServerKey = s.st.Box.Seal(in.Protocol.V.ServerKey)
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
			if h.NodeID == nodeID && h.Port == m.Port && h.InboundID == "" {
				return fmt.Errorf("port %d is used by relay chain %s", m.Port, c.Name)
			}
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
			if h.NodeID == nodeID && h.Port > 0 {
				used[h.Port] = true
			}
		}
	}
	return used, nil
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
