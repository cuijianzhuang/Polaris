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
// Candidate keeps the dial endpoint separate from the TLS server name.
// Discovery metadata describes Shodan's index, not a live geolocation check.
type Candidate struct {
	Address    string `json:"address"`
	ServerName string `json:"serverName,omitempty"`
	ASN        string `json:"asn,omitempty"`
	Org        string `json:"org,omitempty"`
	Country    string `json:"country,omitempty"`
}

type Result struct {
	Address    string   `json:"address,omitempty"`    // actual TCP peer used for validation
	ServerName string   `json:"serverName,omitempty"` // requested/final SNI; Names contains only verified values
	CertNames  []string `json:"certNames,omitempty"`
	ASN        string   `json:"asn,omitempty"`
	Org        string   `json:"org,omitempty"`
	Country    string   `json:"country,omitempty"`
	Target     string   `json:"target"` // dial host:port to use as REALITY "target"
	OK         bool     `json:"ok"`     // usable: TLS 1.3 + h2 + valid certificate
	TLS        string   `json:"tls,omitempty"`
	ALPN       string   `json:"alpn,omitempty"`
	KeyShare   string   `json:"keyShare,omitempty"`
	PQ         bool     `json:"pq"` // X25519MLKEM768 negotiated
	// MLDSA reports whether the certificate chain is long enough for
	// REALITY's ML-DSA-65 signature (it must fit into a record as long as
	// the target's certificate message).
	MLDSA     bool     `json:"mldsa"`
	CertName  string   `json:"certName,omitempty"`
	Issuer    string   `json:"issuer,omitempty"`
	NotAfter  string   `json:"notAfter,omitempty"` // certificate expiry (RFC 3339)
	Names     []string `json:"names,omitempty"`    // only the serverName verified in a live SNI handshake
	ChainSize int      `json:"chainSize,omitempty"`
	LatencyMs *int64   `json:"latencyMs,omitempty"` // nil: no completed final handshake; 0 is a valid measurement
	Problem   string   `json:"problem,omitempty"`
}

const maxHosts = 256

// MLDSAMinChain is the certificate chain size (bytes) a REALITY target needs
// for ML-DSA-65 signatures: the forged certificate carries a 3309-byte
// signature and must not exceed the target's certificate record.
const MLDSAMinChain = 3600

// roots verifies certificates (nil: the system pool); tests replace it.
var roots *x509.CertPool

// maxTotal caps the addresses probed by one scan.
const maxTotal = 512

// Scan probes target: empty (the candidates) or a list separated by commas,
// spaces or newlines of hosts, host:ports, IPs and CIDRs (at most 256
// addresses each, port 443).
func Scan(ctx context.Context, target string) ([]Result, error) {
	tokens := strings.FieldsFunc(target, func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
	if len(tokens) == 0 {
		tokens = Candidates
	}
	var inputs []Candidate
	bulk := map[string]bool{}
	for _, token := range tokens {
		if !strings.Contains(token, "/") {
			inputs = append(inputs, Candidate{Address: token})
		} else {
			p, err := netip.ParsePrefix(token)
			if err != nil {
				return nil, fmt.Errorf("CIDR %s 无效：%w", token, err)
			}
			p = p.Masked()
			for a, n := p.Addr(), 0; p.Contains(a) && n < maxHosts; a, n = a.Next(), n+1 {
				address := net.JoinHostPort(a.String(), "443")
				inputs = append(inputs, Candidate{Address: address})
				bulk[address] = true
			}
		}
		if len(inputs) > maxTotal {
			return nil, fmt.Errorf("一次最多验证 %d 个目标", maxTotal)
		}
	}
	rs, err := ScanCandidates(ctx, inputs)
	if err != nil {
		return nil, err
	}
	out := rs[:0]
	for _, r := range rs {
		if bulk[r.Target] && r.TLS == "" {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// ScanCandidates validates distinct endpoint/SNI pairs with bounded workers.
// A canceled node job never starts a second scan from the panel.
func ScanCandidates(ctx context.Context, inputs []Candidate) ([]Result, error) {
	if len(inputs) > maxTotal {
		return nil, fmt.Errorf("一次最多验证 %d 个目标", maxTotal)
	}
	var targets []Candidate
	seen := map[string]bool{}
	for _, c := range inputs {
		address, _, err := scanAddress(c.Address)
		if err != nil {
			return nil, err
		}
		c.Address = address
		c.ServerName = strings.ToLower(strings.TrimSpace(c.ServerName))
		if c.ServerName != "" && !validHostname(c.ServerName) {
			return nil, errors.New("SNI 必须是具体域名，不能包含通配符")
		}
		key := c.Address + "|" + c.ServerName
		if !seen[key] {
			seen[key] = true
			targets = append(targets, c)
		}
	}
	results := make([]Result, len(targets))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for n := 0; n < min(32, len(targets)); n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				results[i] = ProbeCandidate(ctx, targets[i])
			}
		}()
	}
	for i := range targets {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Certificate discovery can converge with an explicit endpoint/SNI pair.
	out := make([]Result, 0, len(results))
	identities := map[string]int{}
	for _, r := range results {
		key := r.Target + "|" + r.ServerName
		if i, exists := identities[key]; exists {
			old := out[i]
			if r.OK && !old.OK || r.OK == old.OK && r.PQ && !old.PQ {
				out[i] = r
			}
			if out[i].ASN == "" {
				out[i].ASN = old.ASN
				if out[i].ASN == "" {
					out[i].ASN = r.ASN
				}
			}
			if out[i].Org == "" {
				out[i].Org = old.Org
				if out[i].Org == "" {
					out[i].Org = r.Org
				}
			}
			if out[i].Country == "" {
				out[i].Country = old.Country
				if out[i].Country == "" {
					out[i].Country = r.Country
				}
			}
			continue
		}
		identities[key] = len(out)
		out = append(out, r)
	}
	results = out
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].OK != results[j].OK {
			return results[i].OK
		}
		if results[i].PQ != results[j].PQ {
			return results[i].PQ
		}
		if results[i].LatencyMs == nil || results[j].LatencyMs == nil {
			return results[i].LatencyMs != nil
		}
		return *results[i].LatencyMs < *results[j].LatencyMs
	})
	return results, nil
}

func scanAddress(input string) (string, string, error) {
	host, port := strings.TrimSpace(input), "443"
	if h, p, err := net.SplitHostPort(host); err == nil {
		host, port = h, p
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || (net.ParseIP(host) == nil && !validHostname(host)) {
		return "", "", fmt.Errorf("目标地址无效：%s", input)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), host, nil
}

// Probe checks one host or host:port, discovering a concrete SNI for IPs.
func Probe(ctx context.Context, hostport string) Result {
	return ProbeCandidate(ctx, Candidate{Address: hostport})
}

func connectTLS(ctx context.Context, address, sni string) (*tls.Conn, int64, error) {
	start := time.Now()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, 0, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	conn := tls.Client(raw, &tls.Config{ServerName: sni, InsecureSkipVerify: true, // verified against the live SNI below
		NextProtos: []string{"h2", "http/1.1"}, CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519}, MinVersion: tls.VersionTLS12})
	if err := conn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, 0, err
	}
	return conn, time.Since(start).Milliseconds(), nil
}

// ProbeCandidate always keeps the original dial target. An IP's certificate
// is only a hint: reconnect to the SAME IP with the discovered SNI before
// checking certificate validity, TLS and ALPN or allowing a one-click pick.
func ProbeCandidate(ctx context.Context, c Candidate) Result {
	r := Result{Target: c.Address, ASN: c.ASN, Org: c.Org, Country: c.Country}
	address, host, err := scanAddress(c.Address)
	if err != nil {
		r.Problem = err.Error()
		return r
	}
	r.Target = address
	sni := strings.ToLower(strings.TrimSpace(c.ServerName))
	if sni != "" && !validHostname(sni) {
		r.Problem = "SNI 必须是具体域名，不能包含通配符"
		return r
	}
	if sni == "" && net.ParseIP(host) == nil {
		sni = host
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	r.ServerName = sni
	conn, ms, err := connectTLS(ctx, address, sni)
	if err != nil {
		r.Problem = shortErr(err)
		return r
	}
	r.Address = conn.RemoteAddr().String()
	st := conn.ConnectionState()
	if sni == "" {
		fillCertificate(&r, st)
		if len(st.PeerCertificates) > 0 {
			for _, name := range st.PeerCertificates[0].DNSNames {
				name = strings.ToLower(name)
				if validHostname(name) {
					sni = name
					break
				}
			}
		}
		conn.Close()
		if sni == "" {
			r.Problem = "证书中没有可用域名"
			return r
		}
		r.ServerName = sni
		conn, ms, err = connectTLS(ctx, r.Address, sni)
		if err != nil {
			r.Problem = "SNI 重新握手失败：" + shortErr(err)
			return r
		}
		st = conn.ConnectionState()
	}
	defer conn.Close()
	r.ServerName = sni
	r.LatencyMs = &ms // TCP connection plus the final SNI TLS handshake
	fillCertificate(&r, st)
	if len(st.PeerCertificates) == 0 {
		r.Problem = "没有证书"
		return r
	}
	inter := x509.NewCertPool()
	for _, cert := range st.PeerCertificates[1:] {
		inter.AddCert(cert)
	}
	if _, err := st.PeerCertificates[0].Verify(x509.VerifyOptions{DNSName: sni, Intermediates: inter, Roots: roots}); err != nil {
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
		r.Names = []string{sni}
		if !r.PQ {
			r.Problem = "不支持 X25519MLKEM768（可用，但不是最佳）"
		}
	}
	_, port, _ := net.SplitHostPort(address)
	if r.Problem == "" && port != "443" {
		r.Problem = "非 443 端口"
	}
	return r
}

func fillCertificate(r *Result, st tls.ConnectionState) {
	r.TLS = map[uint16]string{tls.VersionTLS13: "1.3", tls.VersionTLS12: "1.2"}[st.Version]
	r.ALPN = st.NegotiatedProtocol
	r.KeyShare = st.CurveID.String()
	r.PQ = st.CurveID == tls.X25519MLKEM768
	r.ChainSize = 0
	for _, cert := range st.PeerCertificates {
		r.ChainSize += len(cert.Raw)
	}
	r.MLDSA = r.ChainSize >= MLDSAMinChain
	if len(st.PeerCertificates) == 0 {
		return
	}
	leaf := st.PeerCertificates[0]
	r.CertName = leaf.Subject.CommonName
	if r.CertName == "" && len(leaf.DNSNames) > 0 {
		r.CertName = leaf.DNSNames[0]
	}
	r.Issuer = leaf.Issuer.CommonName
	if len(leaf.Issuer.Organization) > 0 {
		r.Issuer = leaf.Issuer.Organization[0]
	}
	r.NotAfter = leaf.NotAfter.UTC().Format(time.RFC3339)
	r.CertNames = append([]string(nil), leaf.DNSNames[:min(40, len(leaf.DNSNames))]...)
}

func shortErr(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "连接超时"
	case strings.Contains(err.Error(), "refused"):
		return "连接被拒绝"
	case strings.Contains(err.Error(), "no such host") || strings.Contains(err.Error(), "server misbehaving"):
		return "域名解析失败"
	case strings.Contains(err.Error(), "network is unreachable") || strings.Contains(err.Error(), "no route to host"):
		return "网络不可达"
	case strings.Contains(err.Error(), "connection reset"):
		return "连接被重置"
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
