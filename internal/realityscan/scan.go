// Package realityscan probes TLS servers for use as a REALITY target: TLS 1.3,
// HTTP/2, the X25519MLKEM768 key exchange and a certificate that is valid for
// the name. Scanning an IP or CIDR range discovers names from certificates.
package realityscan

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Candidates are probed when no target is given.
var Candidates = []string{
	"www.microsoft.com", "www.apple.com", "www.amazon.com", "aws.amazon.com", "www.cloudflare.com",
	"www.samsung.com", "www.amd.com", "www.nvidia.com", "www.sony.com", "www.oracle.com",
	"www.cisco.com", "www.intel.com", "www.lovelive-anime.jp", "dl.google.com", "www.yahoo.com",
}

// Result describes one probed target.
type Result struct {
	Target   string `json:"target"` // host:port to use as REALITY "target"
	OK       bool   `json:"ok"`     // usable: TLS 1.3 + h2 + valid certificate
	TLS      string `json:"tls,omitempty"`
	ALPN     string `json:"alpn,omitempty"`
	KeyShare string `json:"keyShare,omitempty"`
	PQ       bool   `json:"pq"` // X25519MLKEM768 negotiated
	// MLDSA reports whether the certificate chain is long enough for
	// REALITY's ML-DSA-65 signature (it must fit into a record as long as
	// the target's certificate message).
	MLDSA     bool     `json:"mldsa"`
	CertName  string   `json:"certName,omitempty"`
	Names     []string `json:"names,omitempty"` // certificate names usable as serverNames
	ChainSize int      `json:"chainSize,omitempty"`
	LatencyMs int64    `json:"latencyMs,omitempty"`
	Problem   string   `json:"problem,omitempty"`
}

const maxHosts = 256

// MLDSAMinChain is the certificate chain size (bytes) a REALITY target needs
// for ML-DSA-65 signatures: the forged certificate carries a 3309-byte
// signature and must not exceed the target's certificate record.
const MLDSAMinChain = 3600

// roots verifies certificates (nil: the system pool); tests replace it.
var roots *x509.CertPool

// Scan probes target: empty (the candidates), a host or host:port, an IP,
// or a CIDR (at most 256 addresses, port 443).
func Scan(ctx context.Context, target string) ([]Result, error) {
	target = strings.TrimSpace(target)
	var hosts []string
	switch {
	case target == "":
		hosts = Candidates
	case strings.Contains(target, "/"):
		p, err := netip.ParsePrefix(target)
		if err != nil {
			return nil, fmt.Errorf("CIDR 无效：%w", err)
		}
		p = p.Masked()
		for a := p.Addr(); p.Contains(a) && len(hosts) < maxHosts; a = a.Next() {
			hosts = append(hosts, a.String())
		}
	default:
		hosts = []string{target}
	}
	results := make([]Result, len(hosts))
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = Probe(ctx, h)
		}()
	}
	wg.Wait()
	if len(hosts) > 1 && strings.Contains(target, "/") {
		// Only answers from a range are interesting.
		out := results[:0]
		for _, r := range results {
			if r.TLS != "" {
				out = append(out, r)
			}
		}
		results = out
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].OK != results[j].OK {
			return results[i].OK
		}
		if results[i].PQ != results[j].PQ {
			return results[i].PQ
		}
		return results[i].LatencyMs < results[j].LatencyMs
	})
	return results, nil
}

// Probe checks one host or host:port.
func Probe(ctx context.Context, hostport string) Result {
	host, port := hostport, "443"
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host, port = h, p
	}
	r := Result{Target: net.JoinHostPort(host, port)}
	isIP := net.ParseIP(host) != nil
	sni := host
	if isIP {
		sni = ""
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	start := time.Now()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", r.Target)
	if err != nil {
		r.Problem = shortErr(err)
		return r
	}
	r.LatencyMs = time.Since(start).Milliseconds()
	conn := tls.Client(raw, &tls.Config{
		ServerName: sni, InsecureSkipVerify: true, // verified below, against the name
		NextProtos:       []string{"h2", "http/1.1"},
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519},
		MinVersion:       tls.VersionTLS12,
	})
	defer conn.Close()
	if err := conn.HandshakeContext(ctx); err != nil {
		r.Problem = shortErr(err)
		return r
	}
	st := conn.ConnectionState()
	r.TLS = map[uint16]string{tls.VersionTLS13: "1.3", tls.VersionTLS12: "1.2"}[st.Version]
	r.ALPN = st.NegotiatedProtocol
	r.KeyShare = st.CurveID.String()
	r.PQ = st.CurveID == tls.X25519MLKEM768
	for _, c := range st.PeerCertificates {
		r.ChainSize += len(c.Raw)
	}
	r.MLDSA = r.ChainSize >= MLDSAMinChain
	if len(st.PeerCertificates) == 0 {
		r.Problem = "没有证书"
		return r
	}
	leaf := st.PeerCertificates[0]
	r.CertName = leaf.Subject.CommonName
	if r.CertName == "" && len(leaf.DNSNames) > 0 {
		r.CertName = leaf.DNSNames[0]
	}
	for _, n := range leaf.DNSNames {
		if !strings.HasPrefix(n, "*.") {
			r.Names = append(r.Names, n)
		}
	}
	if len(r.Names) > 40 {
		r.Names = r.Names[:40]
	}
	verifyName := host
	if isIP {
		if len(r.Names) == 0 {
			r.Problem = "证书中没有可用域名"
			return r
		}
		verifyName = r.Names[0]
		r.Target = net.JoinHostPort(verifyName, port)
	}
	inter := x509.NewCertPool()
	for _, c := range st.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: verifyName, Intermediates: inter, Roots: roots}); err != nil {
		r.Problem = "证书对该域名无效"
		return r
	}
	switch {
	case st.Version != tls.VersionTLS13:
		r.Problem = "不支持 TLS 1.3"
	case r.ALPN != "h2":
		r.Problem = "不支持 HTTP/2"
	default:
		r.OK = true
		if !r.PQ {
			r.Problem = "不支持 X25519MLKEM768（可用，但不是最佳）"
		}
	}
	if r.Problem == "" && port != "443" {
		if p, _ := strconv.Atoi(port); p != 443 {
			r.Problem = "非 443 端口"
		}
	}
	return r
}

func shortErr(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "连接超时"
	case strings.Contains(err.Error(), "refused"):
		return "连接被拒绝"
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
