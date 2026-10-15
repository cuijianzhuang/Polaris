package panel

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// findOrCreateSelfSigned returns a self-signed certificate for host.
func (s *Server) findOrCreateSelfSigned(host string) (*store.Certificate, error) {
	var c store.Certificate
	err := s.st.DB.Where("domain = ? AND provider = ?", host, "selfsigned").First(&c).Error
	if err == nil {
		return &c, nil
	}
	return s.issueSelfSigned(host)
}

func (s *Server) issueSelfSigned(host string) (*store.Certificate, error) {
	certPEM, keyPEM, err := pki.SelfSigned([]string{host}, 3*365*24*time.Hour)
	if err != nil {
		return nil, err
	}
	exp, _ := pki.ParseCertExpiry(certPEM)
	c := store.Certificate{ID: store.NewID(), Domain: host, Provider: "selfsigned", Cert: string(certPEM),
		Key: s.st.Box.Seal(string(keyPEM)), ExpireAt: &exp, AutoRenew: true}
	if err := s.st.DB.Create(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

// acmeHTTPReady reports whether ACME HTTP-01 issuance is possible (only an
// account email is needed; a node answers the challenge).
func (s *Server) acmeHTTPReady() bool { return s.st.GetSetting("acme_email", "") != "" }

// acmeConfigured reports whether ACME DNS-01 issuance is possible.
func (s *Server) acmeConfigured() bool {
	return s.st.GetSetting("acme_email", "") != "" && s.st.Box.MustOpen(s.st.GetSetting("acme_cf_token", "")) != ""
}

type acmeUser struct {
	email string
	reg   *registration.Resource
	key   crypto.PrivateKey
}

func (u *acmeUser) GetEmail() string                        { return u.email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.reg }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

// acmeOpts selects the challenge: "dns" (Cloudflare DNS-01, needed for
// wildcards), "http" (HTTP-01 answered by NodeID on port 80) or "" (DNS when
// a Cloudflare token is set, else HTTP).
type acmeOpts struct {
	Challenge string
	NodeID    string
}

// issueACME obtains a certificate and stores it.
func (s *Server) issueACME(domain string, existing *store.Certificate, opt acmeOpts) (*store.Certificate, error) {
	email := s.st.GetSetting("acme_email", "")
	token := s.st.Box.MustOpen(s.st.GetSetting("acme_cf_token", ""))
	if email == "" {
		return nil, errors.New("ACME 未配置：请在「设置」填写 ACME 邮箱")
	}
	isIP := net.ParseIP(domain) != nil
	if opt.Challenge == "" {
		opt.Challenge = "http"
		if token != "" && !isIP {
			opt.Challenge = "dns"
		}
	}
	if isIP {
		opt.Challenge = "http" // IP certificates can only be validated over HTTP
	}
	switch opt.Challenge {
	case "dns":
		if token == "" {
			return nil, errors.New("DNS 验证需要在「设置」填写 Cloudflare DNS Token")
		}
	case "http":
		if strings.HasPrefix(domain, "*.") {
			return nil, errors.New("泛域名证书只能用 DNS 验证（Cloudflare）")
		}
		if opt.NodeID == "" {
			opt.NodeID = s.nodeForDomain(domain)
		}
		if opt.NodeID == "" {
			return nil, fmt.Errorf("找不到 %s 对应的节点：请选择由哪个节点应答 HTTP 验证（该域名需解析到这个节点）", domain)
		}
		if !s.hub.Online(opt.NodeID) {
			return nil, errors.New("应答 HTTP 验证的节点不在线")
		}
	default:
		return nil, fmt.Errorf("unknown challenge %q", opt.Challenge)
	}
	user := &acmeUser{email: email}
	if raw := s.st.Box.MustOpen(s.st.GetSetting("acme_account_key", "")); raw != "" {
		if blk, _ := pem.Decode([]byte(raw)); blk != nil {
			if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
				user.key = k
			}
		}
	}
	if user.key == nil {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, _ := x509.MarshalECPrivateKey(k)
		_ = s.st.SetSetting("acme_account_key", s.st.Box.Seal(string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))))
		user.key = k
	}
	if raw := s.st.GetSetting("acme_registration", ""); raw != "" {
		var r registration.Resource
		if json.Unmarshal([]byte(raw), &r) == nil {
			user.reg = &r
		}
	}
	cfg := lego.NewConfig(user)
	cfg.CADirURL = s.st.GetSetting("acme_directory", lego.LEDirectoryProduction)
	cfg.Certificate.KeyType = certcrypto.EC256
	client, err := lego.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if opt.Challenge == "dns" {
		err = client.Challenge.SetDNS01Provider(&cfDNSProvider{cf: newCF(token)})
	} else {
		err = client.Challenge.SetHTTP01Provider(&agentHTTP01{s: s, nodeID: opt.NodeID, port: s.st.GetSetting("acme_http_port", "80")})
	}
	if err != nil {
		return nil, err
	}
	if user.reg == nil {
		reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return nil, fmt.Errorf("acme register: %w", err)
		}
		user.reg = reg
		b, _ := json.Marshal(reg)
		_ = s.st.SetSetting("acme_registration", string(b))
	}
	req := certificate.ObtainRequest{Domains: []string{domain}, Bundle: true}
	if isIP {
		req.Profile = "shortlived" // Let's Encrypt only issues IP certificates (about 6 days) under this profile
	}
	res, err := client.Certificate.Obtain(req)
	if err != nil {
		return nil, fmt.Errorf("acme obtain %s: %w", domain, err)
	}
	exp, _ := pki.ParseCertExpiry(res.Certificate)
	c := existing
	if c == nil {
		c = &store.Certificate{ID: store.NewID(), Domain: domain, Provider: "acme", AutoRenew: true}
	}
	c.Cert, c.Key, c.ExpireAt, c.LastError = string(res.Certificate), s.st.Box.Seal(string(res.PrivateKey)), &exp, ""
	c.Challenge, c.NodeID = opt.Challenge, opt.NodeID
	if err := s.st.DB.Save(c).Error; err != nil {
		return nil, err
	}
	return c, nil
}

// cfDNSProvider solves DNS-01 with our own Cloudflare client.
type cfDNSProvider struct {
	cf  *cfClient
	ids map[string]string
}

func (p *cfDNSProvider) Present(domain, _, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fqdn := strings.TrimSuffix(info.EffectiveFQDN, ".")
	zone, err := p.cf.zoneFor(ctx, fqdn)
	if err != nil {
		return err
	}
	var rec struct{ ID string }
	if err := p.cf.do(ctx, http.MethodPost, "/zones/"+zone+"/dns_records", map[string]any{
		"type": "TXT", "name": fqdn, "content": info.Value, "ttl": 120}, &rec); err != nil {
		return err
	}
	if p.ids == nil {
		p.ids = map[string]string{}
	}
	p.ids[fqdn+"|"+info.Value] = zone + "/" + rec.ID
	return nil
}

func (p *cfDNSProvider) CleanUp(domain, _, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)
	key := strings.TrimSuffix(info.EffectiveFQDN, ".") + "|" + info.Value
	ref, ok := p.ids[key]
	if !ok {
		return nil
	}
	parts := strings.SplitN(ref, "/", 2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return p.cf.do(ctx, http.MethodDelete, "/zones/"+parts[0]+"/dns_records/"+parts[1], nil, nil)
}

// certForPolicy resolves a template cert policy for a domain.
func (s *Server) certForPolicy(policy, domain string) (*store.Certificate, string, error) {
	switch {
	case policy == "acme":
		var c store.Certificate
		if err := s.st.DB.Where("domain = ? AND provider = ?", domain, "acme").First(&c).Error; err == nil {
			return &c, "ACME（已有）", nil
		}
		if domain != "" && (s.acmeConfigured() || (s.acmeHTTPReady() && s.nodeForDomain(domain) != "")) {
			c, err := s.issueACME(domain, nil, acmeOpts{})
			if err != nil {
				return nil, "", err
			}
			return c, "ACME 签发", nil
		}
		c2, err := s.findOrCreateSelfSigned(orDefault(domain, "polaris.local"))
		return c2, "自签（未配置 ACME）", err
	case len(policy) > 9 && policy[:9] == "wildcard:":
		var c store.Certificate
		if err := s.st.DB.First(&c, "id = ?", policy[9:]).Error; err != nil {
			return nil, "", fmt.Errorf("wildcard certificate: %w", err)
		}
		return &c, "泛域名证书", nil
	default:
		c, err := s.findOrCreateSelfSigned(orDefault(domain, "polaris.local"))
		return c, "自签", err
	}
}

// renewCertificates renews certificates that have used two thirds of their
// lifetime (30 days before expiry for 90-day certificates, 2 days for the
// 6-day IP certificates, which keeps those under Let's Encrypt's duplicate
// certificate limit) and pushes them to every node that uses them.
func (s *Server) renewCertificates() {
	var cs []store.Certificate
	s.st.DB.Where("auto_renew = ? AND expire_at < ?", true, time.Now().Add(30*24*time.Hour)).Find(&cs)
	for i := range cs {
		if !renewDue(&cs[i], time.Now()) {
			continue
		}
		if err := s.renewCertificate(&cs[i]); err != nil {
			s.log.Error("renew certificate", "domain", cs[i].Domain, "err", err)
		}
	}
}

// renewDue reports whether less than a third of the certificate's lifetime
// (capped at 30 days) remains.
func renewDue(c *store.Certificate, now time.Time) bool {
	if c.ExpireAt == nil {
		return true
	}
	window := 30 * 24 * time.Hour
	if blk, _ := pem.Decode([]byte(c.Cert)); blk != nil {
		if leaf, err := x509.ParseCertificate(blk.Bytes); err == nil {
			if third := leaf.NotAfter.Sub(leaf.NotBefore) / 3; third < window {
				window = third
			}
		}
	}
	return c.ExpireAt.Sub(now) < window
}

func (s *Server) renewCertificate(c *store.Certificate) error {
	switch c.Provider {
	case "acme":
		if _, err := s.issueACME(c.Domain, c, acmeOpts{Challenge: c.Challenge, NodeID: c.NodeID}); err != nil {
			s.st.DB.Model(c).Update("last_error", truncate(err.Error(), 1900))
			s.notify(Event{Kind: "cert_failed", Text: "🔐 证书 " + c.Domain + " 续期失败：" + truncate(err.Error(), 200)})
			return err
		}
	case "selfsigned":
		certPEM, keyPEM, err := pki.SelfSigned([]string{c.Domain}, 3*365*24*time.Hour)
		if err != nil {
			return err
		}
		exp, _ := pki.ParseCertExpiry(certPEM)
		c.Cert, c.Key, c.ExpireAt = string(certPEM), s.st.Box.Seal(string(keyPEM)), &exp
		if err := s.st.DB.Save(c).Error; err != nil {
			return err
		}
	default:
		return fmt.Errorf("provider %s cannot be renewed automatically", c.Provider)
	}
	s.audit("system", "cert.renew", c.Domain, c.Provider, "")
	s.MarkDirty("certificate renewed", s.nodesUsingCert(c.ID)...)
	return nil
}

// nodesUsingCert finds nodes with inbounds referencing the certificate.
func (s *Server) nodesUsingCert(certID string) []string {
	var ins []store.Inbound
	s.st.DB.Find(&ins)
	var ids []string
	for _, in := range ins {
		if in.Security.V.CertificateID == certID {
			ids = append(ids, in.NodeID)
		}
	}
	return ids
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
