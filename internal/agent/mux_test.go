package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"strings"
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
		Routes:   []model.MuxRoute{{SNI: "a.test", Upstream: a, Inbound: "a"}, {SNI: "b.test", Upstream: b, Inbound: "b"}},
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
