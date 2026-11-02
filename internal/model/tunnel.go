package model

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const ManagedTunnelFeature = "cloudflare-tunnel-v1"
const RemoteTunnelFeature = "cloudflare-tunnel-token-v1"
const RemoteTunnelMinVersion = "2025.4.0" // --token-file, Cloudflare official minimum
const CloudflaredVersion = "2026.10.7"

type TunnelRoute struct {
	Hostname      string        `json:"hostname,omitempty" yaml:"hostname,omitempty"`
	Path          string        `json:"path,omitempty" yaml:"path,omitempty"`
	Service       string        `json:"service" yaml:"service"`
	OriginRequest *TunnelOrigin `json:"originRequest,omitempty" yaml:"originRequest,omitempty"`
}
type TunnelOrigin struct {
	HTTPHostHeader string `json:"httpHostHeader" yaml:"httpHostHeader"`
}
type TunnelSpec struct {
	ProfileID         string        `json:"profileId"`
	ID                string        `json:"id"`
	CredentialVersion string        `json:"credentialVersion"`
	CredentialMode    string        `json:"credentialMode,omitempty"` // empty/local = local JSON, remote = token file
	Routes            []TunnelRoute `json:"routes"`
}

var tunnelUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var tunnelProfile = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var tunnelVersion = regexp.MustCompile(`^[a-z2-9]{16}$`)

func (t TunnelSpec) Validate() error {
	if !tunnelProfile.MatchString(t.ProfileID) || !tunnelUUID.MatchString(t.ID) || !tunnelVersion.MatchString(t.CredentialVersion) {
		return fmt.Errorf("invalid managed tunnel identity")
	}
	if t.CredentialMode != "" && t.CredentialMode != "local" && t.CredentialMode != "remote" {
		return fmt.Errorf("invalid tunnel credential mode")
	}
	if t.CredentialMode == "remote" {
		if len(t.Routes) != 0 {
			return fmt.Errorf("remote tunnel routes must come from Cloudflare")
		}
		return nil
	}
	if len(t.Routes) < 2 || len(t.Routes) > 1025 {
		return fmt.Errorf("invalid managed tunnel routes")
	}
	for i, r := range t.Routes {
		if i == len(t.Routes)-1 {
			if r.Service != "http_status:404" || r.Hostname != "" || r.Path != "" {
				return fmt.Errorf("managed tunnel requires a 404 fallback")
			}
			continue
		}
		if r.Hostname == "" || strings.ContainsAny(r.Hostname, "\r\n/ :*\t") || r.Path == "" {
			return fmt.Errorf("invalid tunnel hostname/path")
		}
		if _, err := regexp.Compile(r.Path); err != nil {
			return fmt.Errorf("invalid tunnel path expression")
		}
		u, err := url.Parse(r.Service)
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "" || u.RawQuery != "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("managed tunnel origin must be local HTTP")
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid tunnel origin port")
		}
		if r.OriginRequest != nil && strings.ContainsAny(r.OriginRequest.HTTPHostHeader, "\r\n") {
			return fmt.Errorf("invalid tunnel origin Host")
		}
	}
	return nil
}

func NormalizeTunnelCredentials(raw []byte, id string) ([]byte, error) {
	invalid := fmt.Errorf("需要匹配 Tunnel UUID 的本地管理凭据 JSON（AccountTag、TunnelID、TunnelSecret），不是远程 Token")
	if len(raw) == 0 || len(raw) > 16384 || !tunnelUUID.MatchString(id) {
		return nil, invalid
	}
	var c struct {
		AccountTag   string
		TunnelID     string
		TunnelSecret string
	}
	if json.Unmarshal(raw, &c) != nil || strings.ToLower(c.TunnelID) != id {
		return nil, invalid
	}
	account, err := hex.DecodeString(c.AccountTag)
	if err != nil || len(account) != 16 {
		return nil, invalid
	}
	secret, err := base64.StdEncoding.DecodeString(c.TunnelSecret)
	if err != nil || len(secret) < 32 || len(secret) > 256 {
		return nil, invalid
	}
	c.AccountTag = strings.ToLower(c.AccountTag)
	c.TunnelID = id
	return json.Marshal(c)
}

// Validate identity without converting a remotely managed token into local
// credentials. The original opaque token is what cloudflared must receive.
func NormalizeRemoteTunnelToken(raw []byte, id string) ([]byte, error) {
	invalid := fmt.Errorf("请填写完整 Tunnel Token（不是安装命令或 Cloudflare API Token），且必须匹配配置档 UUID")
	if len(raw) > 16384 || !tunnelUUID.MatchString(id) {
		return nil, invalid
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, invalid
	}
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return nil, invalid
	}
	var c struct {
		Account string `json:"a"`
		ID      string `json:"t"`
		Secret  []byte `json:"s"`
	}
	if json.Unmarshal(decoded, &c) != nil || strings.ToLower(c.ID) != id {
		return nil, invalid
	}
	a, err := hex.DecodeString(c.Account)
	if err != nil || len(a) != 16 || len(c.Secret) < 32 || len(c.Secret) > 256 {
		return nil, invalid
	}
	return []byte(token), nil
}

func NormalizeManagedTunnelCredential(raw []byte, id, mode string) ([]byte, error) {
	if mode == "remote" {
		return NormalizeRemoteTunnelToken(raw, id)
	}
	if mode != "" && mode != "local" {
		return nil, fmt.Errorf("invalid tunnel credential mode")
	}
	return NormalizeTunnelCredentials(raw, id)
}
