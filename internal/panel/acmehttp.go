package panel

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// agentHTTP01 solves ACME HTTP-01 by asking the node the domain points to to
// serve the token on port 80 (the agent's temporary challenge server).
type agentHTTP01 struct {
	s      *Server
	nodeID string
	port   string
}

func (p *agentHTTP01) call(action, token, keyAuth string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	res, err := p.s.hub.Command(ctx, p.nodeID, "acme_http",
		map[string]string{"action": action, "token": token, "keyauth": keyAuth, "port": p.port}, 30*time.Second)
	if err != nil {
		return err
	}
	if !res.Ok {
		return errors.New(res.Error)
	}
	return nil
}

func (p *agentHTTP01) Present(_, token, keyAuth string) error {
	return p.call("present", token, keyAuth)
}
func (p *agentHTTP01) CleanUp(_, token, keyAuth string) error {
	return p.call("cleanup", token, keyAuth)
}

// nodeForDomain finds the node a domain or IP points to: its domain or
// address, or a DDNS binding.
func (s *Server) nodeForDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return ""
	}
	var nodes []store.Node
	s.st.DB.Find(&nodes)
	for _, n := range nodes {
		for _, v := range []string{n.Domain, n.Address, n.PublicIPv4, n.PublicIPv6} {
			if v != "" && strings.EqualFold(v, domain) {
				return n.ID
			}
		}
	}
	var b store.DomainBinding
	if s.st.DB.Where("LOWER(domain) = ?", domain).First(&b).Error == nil {
		return b.NodeID
	}
	return ""
}
