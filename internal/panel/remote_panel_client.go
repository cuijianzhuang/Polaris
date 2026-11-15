package panel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// remotePanelClient fetches from one member panel.
type remotePanelClient struct {
	baseURL  string
	token    string
	caPin    string
	insecure bool
	timeout  time.Duration
	rt       http.RoundTripper // tests: bypass the SSRF dialer for loopback members
}

// ssrfDialer resolves the address and rejects non-public targets before
// dialing, so DNS rebinding cannot bypass the check made at registration.
func ssrfDialer(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if err := checkSSRF(ip); err != nil {
				return nil, fmt.Errorf("blocked target %s: %w", host, err)
			}
		}
		return dialer.DialContext(ctx, network, addr)
	}
}

func (c *remotePanelClient) client() *http.Client {
	if c.rt != nil {
		return &http.Client{Transport: c.rt, Timeout: c.timeout}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.insecure},
		DialContext:     ssrfDialer(dialer),
	}
	if c.caPin != "" && !c.insecure {
		tr.TLSClientConfig.InsecureSkipVerify = true
		tr.TLSClientConfig.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("no certificate")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			h := sha256.Sum256(cert.Raw)
			pin := hex.EncodeToString(h[:])
			if pin != c.caPin {
				return fmt.Errorf("certificate pin mismatch: got %s, want %s", pin, c.caPin)
			}
			return nil
		}
	}
	return &http.Client{Transport: tr, Timeout: c.timeout}
}

func (c *remotePanelClient) get(ctx context.Context, path string, out any) (status int, err error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return 0, fmt.Errorf("parse base URL: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = ""

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB cap
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// fetchSnapshot calls GET /panel/self on the member.
func (c *remotePanelClient) fetchSnapshot(ctx context.Context) (*store.RemoteSnapshot, error) {
	var snap store.RemoteSnapshot
	_, err := c.get(ctx, "/api/panel/self", &snap)
	return &snap, err
}

// post sends a JSON body to the member and decodes the response.
func (c *remotePanelClient) post(ctx context.Context, path string, body any, out any) (status int, err error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return 0, fmt.Errorf("parse base URL: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = ""

	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// checkSSRF rejects targets that must never be reachable from the hub.
// Private ranges stay allowed: operators manage on-premise panels.
func checkSSRF(ip net.IP) error {
	if ip.IsLoopback() {
		return errors.New("loopback address")
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return errors.New("link-local address")
	}
	if ip.Equal(net.ParseIP("169.254.169.254")) {
		return errors.New("cloud metadata address")
	}
	return nil
}
