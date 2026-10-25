// Package core contains the shared fixed-target business probes. HTTP uses
// normal certificate verification and never follows a redirect or a direct
// fallback; UDP verifies a fresh DNS transaction and its exact question.
package core

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"github.com/miekg/dns"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func DiagnosticPort() (int, error) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return 0, e
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func DiagnosticHTTP(ctx context.Context, target string, port int) error {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return err
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	req.Header.Set("X-Polaris-Diagnostic", hex.EncodeToString(nonce))
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("HTTP status %d", res.StatusCode)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	return err
}

func DiagnosticDNS(ctx context.Context, port int) error {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	name := hex.EncodeToString(nonce) + ".invalid."
	query := new(dns.Msg)
	query.SetQuestion(name, dns.TypeA)
	wire, err := query.Pack()
	if err != nil {
		return err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(4 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err = conn.Write(wire); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	var response dns.Msg
	if err = response.Unpack(buf[:n]); err != nil {
		return err
	}
	if !response.Response || response.Id != query.Id || len(response.Question) != 1 || !strings.EqualFold(response.Question[0].Name, name) || response.Question[0].Qtype != dns.TypeA || (response.Rcode != dns.RcodeSuccess && response.Rcode != dns.RcodeNameError) {
		return fmt.Errorf("invalid DNS reply")
	}
	return nil
}
