package agent

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func muxTestCert(t *testing.T, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]},
		DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// tlsBackend serves TLS and answers with marker after the handshake.
func tlsBackend(t *testing.T, marker string, names ...string) string {
	t.Helper()
	cert := muxTestCert(t, names...)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if tc, ok := c.(*tls.Conn); ok {
					_ = tc.Handshake()
				}
				_, _ = io.WriteString(c, marker)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// echoBackend answers every connection with "FB:" + whatever it receives.
func echoBackend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Accumulate until a short idle gap: the mux replays its
				// buffered prefix first and streams the rest after, so one
				// Read is not the whole message.
				var data []byte
				buf := make([]byte, 4096)
				for {
					_ = c.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
					n, err := c.Read(buf)
					data = append(data, buf[:n]...)
					if err != nil {
						break
					}
				}
				_, _ = io.WriteString(c, "FB:"+string(data))
			}(c)
		}
	}()
	return ln.Addr().String()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func dialTLS(t *testing.T, addr, sni string) string {
	t.Helper()
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr,
		&tls.Config{ServerName: sni, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("dial %s: %v", sni, err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, _ := io.ReadAll(c)
	return string(b)
}

func TestMuxRoutesBySNI(t *testing.T) {
	a := tlsBackend(t, "BACKEND-A", "a.test")
	b := tlsBackend(t, "BACKEND-B", "b.test")
	fb := echoBackend(t)
	port := freePort(t)
	mgr := newMuxManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	spec := &model.NodeSpec{Muxes: []model.MuxSpec{{
		Name: "g", Listen: "127.0.0.1", Port: port,
		Routes: []model.MuxRoute{{SNI: "a.test", Upstream: a, Inbound: "a"}, {SNI: "b.test", Upstream: b, Inbound: "b"},
			{SNI: "fb.test", Upstream: fb, Inbound: "fb"}},
		// The fallback here is a member (its upstream is one of the
		// routes), the batch-1 "member fallback" shape; a non-member
		// loopback fallback is refused by the public dial guard.
		Fallback: fb,
	}}}
	if err := mgr.sync(spec); err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if got := dialTLS(t, addr, "a.test"); got != "BACKEND-A" {
		t.Fatalf("a.test routed to %q", got)
	}
	if got := dialTLS(t, addr, "b.test"); got != "BACKEND-B" {
		t.Fatalf("b.test routed to %q", got)
	}
	// Unknown SNI takes the fallback, with the ClientHello replayed.
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	hello, _ := tlsClientHello(t, "unknown.test")
	if _, err := c.Write(hello); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "FB:") || !strings.Contains(string(buf[:n]), "unknown.test") {
		t.Fatalf("unknown SNI fallback: n=%d err=%v head=%q", n, err, buf[:min(n, 40)])
	}
	// Non-TLS traffic also takes the fallback, bytes intact.
	c2, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_ = c2.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c2, "hello mux world"); err != nil {
		t.Fatal(err)
	}
	n, err = c2.Read(buf)
	if err != nil || string(buf[:n]) != "FB:hello mux world" {
		t.Fatalf("non-TLS fallback: n=%d err=%v got=%q", n, err, buf[:n])
	}
	// Removing the group closes the listener.
	if err := mgr.sync(&model.NodeSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		t.Fatal("listener still up after group removal")
	}
}

func TestMuxNoFallbackDrops(t *testing.T) {
	a := tlsBackend(t, "BACKEND-A", "a.test")
	port := freePort(t)
	mgr := newMuxManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	spec := &model.NodeSpec{Muxes: []model.MuxSpec{{
		Name: "g", Listen: "127.0.0.1", Port: port,
		Routes: []model.MuxRoute{{SNI: "a.test", Upstream: a, Inbound: "a"}},
	}}}
	if err := mgr.sync(spec); err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr,
		&tls.Config{ServerName: "nope.test", InsecureSkipVerify: true})
	if err == nil {
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("unknown SNI without fallback must be dropped")
		}
	}
}

func TestMuxHelloTimeoutReaps(t *testing.T) {
	old := muxHelloTimeout
	muxHelloTimeout = 300 * time.Millisecond
	defer func() { muxHelloTimeout = old }()
	port := freePort(t)
	mgr := newMuxManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	spec := &model.NodeSpec{Muxes: []model.MuxSpec{{
		Name: "g", Listen: "127.0.0.1", Port: port, Fallback: echoBackend(t),
	}}}
	if err := mgr.sync(spec); err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for _, send := range [][]byte{nil, {0x16, 0x03}} { // silent, then partial record header
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if len(send) > 0 {
			_, _ = c.Write(send)
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		start := time.Now()
		_, err = c.Read(make([]byte, 1))
		if err == nil {
			t.Fatal("half-open connection was not reaped")
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("reap took %v, want ~hello timeout", time.Since(start))
		}
		c.Close()
	}
}

// tlsClientHello captures a real ClientHello for sni from a TLS client
// whose handshake is intentionally left unfinished.
func tlsClientHello(t *testing.T, sni string) ([]byte, error) {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		tc := tls.Client(client, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
		_ = tc.Handshake()
	}()
	_ = server.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	n, err := server.Read(buf)
	return buf[:n], err
}

func TestParseClientHelloSNI(t *testing.T) {
	hello, err := tlsClientHello(t, "Example.COM")
	if err != nil {
		t.Fatal(err)
	}
	sni, prefix, isTLS, err := readClientHelloSNI(strings.NewReader(string(hello)))
	if err != nil || !isTLS || sni != "example.com" {
		t.Fatalf("sni=%q isTLS=%v err=%v", sni, isTLS, err)
	}
	if len(prefix) != len(hello) {
		t.Fatalf("prefix %d bytes, hello %d", len(prefix), len(hello))
	}
	// Truncated record: an error, not a fallback.
	if _, _, _, err := readClientHelloSNI(strings.NewReader(string(hello[:len(hello)-3]))); err == nil {
		t.Fatal("truncated hello must error")
	}
	// Garbage that is not TLS: no error, not TLS.
	_, prefix, isTLS, err = readClientHelloSNI(strings.NewReader("GET / HTTP/1.1\r\n\r\n"))
	if err != nil || isTLS || len(prefix) != 5 {
		t.Fatalf("non-tls: prefix=%d isTLS=%v err=%v", len(prefix), isTLS, err)
	}
}

// --- hardening tests (SNI mux audit) ---

// A fallback that is not a member must never receive traffic at a
// loopback address: the guarded public dial refuses it and the
// connection drops, so the splitter cannot be used as an unauthenticated
// relay to services on the node itself.
func TestMuxExternalLoopbackFallbackRefused(t *testing.T) {
	a := tlsBackend(t, "BACKEND-A", "a.test")
	fb := echoBackend(t) // loopback, but NOT one of the routes
	port := freePort(t)
	mgr := newMuxManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	spec := &model.NodeSpec{Muxes: []model.MuxSpec{{
		Name: "g", Listen: "127.0.0.1", Port: port,
		Routes:   []model.MuxRoute{{SNI: "a.test", Upstream: a, Inbound: "a"}},
		Fallback: fb,
	}}}
	if err := mgr.sync(spec); err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if got := dialTLS(t, addr, "a.test"); got != "BACKEND-A" {
		t.Fatalf("member traffic broken: %q", got)
	}
	// Unknown SNI: dropped, not relayed to the loopback fallback.
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	hello, _ := tlsClientHello(t, "unknown.test")
	if _, err := c.Write(hello); err != nil {
		t.Fatal(err)
	}
	n, err := c.Read(make([]byte, 64))
	if err == nil || n != 0 {
		t.Fatalf("unknown SNI reached loopback fallback: n=%d err=%v", n, err)
	}
	// Non-TLS traffic: same refusal.
	c2, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_ = c2.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c2, "hello mux world"); err != nil {
		t.Fatal(err)
	}
	n, err = c2.Read(make([]byte, 64))
	if err == nil || n != 0 {
		t.Fatalf("non-TLS traffic reached loopback fallback: n=%d err=%v", n, err)
	}
}

func TestPublicIP(t *testing.T) {
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "172.32.0.1", "100.128.0.1",
		"2606:4700:4700::1111", "2001:4860:4860::8888"} {
		if !publicIP(net.ParseIP(s)) {
			t.Fatalf("%s must count as public", s)
		}
	}
	for _, s := range []string{"127.0.0.1", "127.1.2.3", "0.0.0.0", "10.0.0.1", "172.16.0.1",
		"172.31.255.255", "192.168.0.1", "169.254.169.254", "100.64.0.1", "100.127.255.255",
		"224.0.0.1", "::1", "::", "fe80::1", "fd00::1", "ff02::1"} {
		if publicIP(net.ParseIP(s)) {
			t.Fatalf("%s must not count as public", s)
		}
	}
	if !isLocalAddr(net.ParseIP("127.0.0.1")) || isLocalAddr(net.ParseIP("8.8.8.8")) {
		t.Fatal("isLocalAddr misclassifies")
	}
}

func TestDialPublicFallbackRefusesNonPublic(t *testing.T) {
	for _, target := range []string{
		"127.0.0.1:443", "localhost:443", "10.0.0.8:443", "192.168.1.1:443",
		"169.254.169.254:80", "100.64.3.2:443", "[::1]:443", "[fe80::1]:443",
		"missing-port", "host:notaport",
	} {
		if _, err := dialPublicFallback(target, 443, 500*time.Millisecond); err == nil {
			t.Fatalf("target %s dialed, want refusal", target)
		}
	}
}

// buildHelloMsg assembles a ClientHello handshake message (type+length
// header included). extLen < 0 means "use len(exts)".
func buildHelloMsg(exts []byte, extLen int, withExts bool) []byte {
	body := []byte{0x03, 0x03}
	body = append(body, make([]byte, 32)...)
	body = append(body, 0)                // session id
	body = append(body, 0, 2, 0x13, 0x01) // cipher suites
	body = append(body, 1, 0)             // compression methods
	if withExts {
		if extLen < 0 {
			extLen = len(exts)
		}
		body = append(body, byte(extLen>>8), byte(extLen))
		body = append(body, exts...)
	}
	msg := []byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	return append(msg, body...)
}

func sniExtension(name string) []byte {
	entry := []byte{0, byte(len(name) >> 8), byte(len(name))}
	entry = append(entry, name...)
	data := append([]byte{byte(len(entry) >> 8), byte(len(entry))}, entry...)
	ext := []byte{0, 0, byte(len(data) >> 8), byte(len(data))}
	return append(ext, data...)
}

// Malformed hellos must be parse errors (design D6: drop), not "no SNI"
// — otherwise a crafted hello is handed to the fallback as if it were a
// legitimate SNI-less client.
func TestParseClientHelloMalformed(t *testing.T) {
	if sni, err := parseClientHelloSNI(buildHelloMsg(sniExtension("Example.ORG"), -1, true)); err != nil || sni != "example.org" {
		t.Fatalf("valid hello: sni=%q err=%v", sni, err)
	}
	// No extensions field at all: no SNI, no error.
	if sni, err := parseClientHelloSNI(buildHelloMsg(nil, -1, false)); err != nil || sni != "" {
		t.Fatalf("no extensions: sni=%q err=%v", sni, err)
	}
	// Extensions length overruns the body.
	if _, err := parseClientHelloSNI(buildHelloMsg(sniExtension("a.test"), 999, true)); err == nil {
		t.Fatal("extensions overrun must error")
	}
	// Trailing junk shorter than an extension header.
	if _, err := parseClientHelloSNI(buildHelloMsg([]byte{0xAA, 0xBB}, -1, true)); err == nil {
		t.Fatal("extension stub must error")
	}
	// server_name list entry truncated to a single dangling byte.
	trunc := []byte{0, 0, 0, 3, 0, 1, 0}
	if _, err := parseClientHelloSNI(buildHelloMsg(trunc, -1, true)); err == nil {
		t.Fatal("server_name stub must error")
	}
	// server_name holding only a non-host_name entry: no SNI, no error.
	entry := []byte{1, 0, 3, 'a', 'b', 'c'}
	data := append([]byte{byte(len(entry) >> 8), byte(len(entry))}, entry...)
	other := append([]byte{0, 0, byte(len(data) >> 8), byte(len(data))}, data...)
	if sni, err := parseClientHelloSNI(buildHelloMsg(other, -1, true)); err != nil || sni != "" {
		t.Fatalf("non-host_name entry: sni=%q err=%v", sni, err)
	}
}

func TestReadClientHelloOversizeDeclared(t *testing.T) {
	record := func(payload []byte) []byte {
		return append([]byte{0x16, 0x03, 0x01, byte(len(payload) >> 8), byte(len(payload))}, payload...)
	}
	// Declared handshake length far beyond the cap: refused from the
	// header alone, without streaming the body first.
	p := make([]byte, 104)
	copy(p, []byte{0x01, 0x01, 0x86, 0xA0}) // msgLen = 100000
	if _, _, _, err := readClientHelloSNI(bytes.NewReader(record(p))); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("declared oversize: err=%v", err)
	}
	// Cumulative records beyond the cap: refused before the crossing
	// record's payload is read (the stream ends after its header).
	chunk := make([]byte, 18432)
	copy(chunk, []byte{0x01, 0x00, 0xEA, 0x60}) // msgLen = 60000, under the cap
	var stream []byte
	for i := 0; i < 3; i++ {
		stream = append(stream, record(chunk)...)
	}
	stream = append(stream, 0x16, 0x03, 0x01, 0x48, 0x00) // 4th record header only
	if _, _, _, err := readClientHelloSNI(bytes.NewReader(stream)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("cumulative oversize: err=%v", err)
	}
}

// flakyListener fails Accept a fixed number of times (the EMFILE shape),
// then serves connections handed over ch.
type flakyListener struct {
	mu     sync.Mutex
	fails  int
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (f *flakyListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return nil, errors.New("too many open files")
	}
	f.mu.Unlock()
	select {
	case c := <-f.ch:
		return c, nil
	case <-f.closed:
		return nil, errors.New("listener closed")
	}
}

func (f *flakyListener) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *flakyListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 443}
}

// A transient accept failure must not silently kill the listener: the
// manager keeps it registered and would never rebind it.
func TestMuxAcceptTransientErrorSurvives(t *testing.T) {
	old := muxAcceptRetry
	muxAcceptRetry = time.Millisecond
	defer func() { muxAcceptRetry = old }()
	backend := tlsBackend(t, "BACKEND-A", "a.test")
	fl := &flakyListener{fails: 5, ch: make(chan net.Conn, 1), closed: make(chan struct{})}
	l := &muxListener{port: 443, ln: fl, sem: make(chan struct{}, 4), conns: map[net.Conn]struct{}{}}
	l.table.Store(muxTable{routes: map[string]string{"a.test": backend}})
	go l.serve(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer l.close()

	clientEnd, serverEnd := net.Pipe()
	fl.ch <- serverEnd
	tc := tls.Client(clientEnd, &tls.Config{ServerName: "a.test", InsecureSkipVerify: true})
	_ = clientEnd.SetDeadline(time.Now().Add(3 * time.Second))
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake through recovered listener: %v", err)
	}
	b, err := io.ReadAll(tc)
	if err != nil || string(b) != "BACKEND-A" {
		t.Fatalf("got %q err %v", b, err)
	}
	tc.Close()
}

func TestPortsOfIncludesMux(t *testing.T) {
	spec := &model.NodeSpec{Muxes: []model.MuxSpec{{Name: "main", Port: 443}}}
	found := false
	for _, p := range portsOf(spec) {
		if p.Proto == "tcp" && p.Port == 443 && strings.Contains(p.Owner, "main") {
			found = true
		}
	}
	if !found {
		t.Fatal("mux port missing from portsOf")
	}
}
