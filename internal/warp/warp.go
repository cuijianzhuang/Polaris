// Package warp registers a free Cloudflare WARP device (as wgcf and the
// WARP apps do) and returns what a WireGuard outbound needs. The private
// key is generated locally and never sent.
package warp

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// API is the WARP client API root; tests point it at a fake server.
var API = "https://api.cloudflareclient.com/v0a2158"

// Port is the WireGuard port of the WARP endpoints (the API reports 0).
const Port = 2408

// MTU is a safe tunnel MTU for WARP.
const MTU = 1280

// Device is a registered WARP device.
type Device struct {
	ID            string `json:"id"`
	PrivateKey    string `json:"-"`
	PublicKey     string `json:"publicKey"`
	PeerPublicKey string `json:"peerPublicKey"`
	Endpoint      string `json:"endpoint"` // host (IPv4 when the API reports one)
	V4, V6        string `json:"-"`        // tunnel addresses
	Reserved      []int  `json:"reserved"` // client id bytes, sent in WireGuard reserved
	AccountType   string `json:"accountType"`
	WarpPlus      bool   `json:"warpPlus"`
}

// LocalAddresses are the tunnel addresses with prefix lengths.
func (d *Device) LocalAddresses() []string {
	var out []string
	if d.V4 != "" {
		out = append(out, d.V4+"/32")
	}
	if d.V6 != "" {
		out = append(out, d.V6+"/128")
	}
	return out
}

type regResponse struct {
	ID      string `json:"id"`
	Token   string `json:"token"`
	Account struct {
		AccountType string `json:"account_type"`
		WarpPlus    bool   `json:"warp_plus"`
	} `json:"account"`
	Config struct {
		ClientID string `json:"client_id"`
		Peers    []struct {
			PublicKey string `json:"public_key"`
			Endpoint  struct {
				V4   string `json:"v4"`
				V6   string `json:"v6"`
				Host string `json:"host"`
			} `json:"endpoint"`
		} `json:"peers"`
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"addresses"`
		} `json:"interface"`
	} `json:"config"`
}

// Register creates a device; license (optional) upgrades it to WARP+.
func Register(ctx context.Context, license string) (*Device, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	pub := base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())
	body := map[string]any{
		"key": pub, "install_id": "", "fcm_token": "", "model": "PC", "serial_number": "", "locale": "en_US",
		"tos": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	var reg regResponse
	if err := call(ctx, http.MethodPost, "/reg", "", body, &reg); err != nil {
		return nil, fmt.Errorf("注册 WARP 失败：%w", err)
	}
	if reg.ID == "" || reg.Token == "" || len(reg.Config.Peers) == 0 || reg.Config.Peers[0].PublicKey == "" {
		return nil, errors.New("注册 WARP 失败：返回的数据不完整")
	}
	d := &Device{ID: reg.ID, PrivateKey: base64.StdEncoding.EncodeToString(priv.Bytes()), PublicKey: pub,
		PeerPublicKey: reg.Config.Peers[0].PublicKey,
		V4:            reg.Config.Interface.Addresses.V4, V6: reg.Config.Interface.Addresses.V6,
		AccountType: reg.Account.AccountType, WarpPlus: reg.Account.WarpPlus}
	if d.V4 == "" && d.V6 == "" {
		return nil, errors.New("注册 WARP 失败：没有分配隧道地址")
	}
	d.Endpoint = endpointHost(reg.Config.Peers[0].Endpoint.V4, reg.Config.Peers[0].Endpoint.Host)
	if id, err := base64.StdEncoding.DecodeString(reg.Config.ClientID); err == nil && len(id) == 3 {
		d.Reserved = []int{int(id[0]), int(id[1]), int(id[2])}
	}
	// Newer devices start with WARP off on the account side.
	_ = call(ctx, http.MethodPatch, "/reg/"+reg.ID, reg.Token, map[string]any{"warp_enabled": true}, nil)
	if license = strings.TrimSpace(license); license != "" {
		var acct struct {
			AccountType string `json:"account_type"`
			WarpPlus    bool   `json:"warp_plus"`
		}
		if err := call(ctx, http.MethodPut, "/reg/"+reg.ID+"/account", reg.Token, map[string]any{"license": license}, &acct); err != nil {
			return nil, fmt.Errorf("绑定 WARP+ 许可证失败：%w", err)
		}
		d.AccountType, d.WarpPlus = acct.AccountType, acct.WarpPlus
	}
	return d, nil
}

// endpointHost prefers the IPv4 address the API reports (no DNS lookup on
// the node), else the host name.
func endpointHost(v4, host string) string {
	if h, _, err := net.SplitHostPort(v4); err == nil && net.ParseIP(h) != nil {
		return h
	}
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		return h
	}
	return "engage.cloudflareclient.com"
}

func call(ctx context.Context, method, path, token string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, method, API+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("User-Agent", "okhttp/3.12.1")
	req.Header.Set("CF-Client-Version", "a-6.30-3596")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("面板服务器连不上 Cloudflare（api.cloudflareclient.com）：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if json.Unmarshal(raw, &e) == nil && len(e.Errors) > 0 && e.Errors[0].Message != "" {
			return fmt.Errorf("HTTP %d：%s", resp.StatusCode, e.Errors[0].Message)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// Some responses wrap the payload in "result".
		var w struct {
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(raw, &w) == nil && len(w.Result) > 0 {
			return json.Unmarshal(w.Result, out)
		}
		return errors.New("返回了无法识别的内容")
	}
	return nil
}
