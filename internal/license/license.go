// Package license verifies Polaris licenses: a JSON document signed with
// Ed25519 by the license server. The panel verifies it offline with the
// public keys built into the binary; the server is only needed to activate,
// deactivate and check for revocation.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Paid features.
const (
	FeatureSpeedLimit = "speed_limit" // per-user speed limits
	FeatureTGMenu     = "tg_menu"     // Telegram admin menus
	FeatureBackupPush = "backup_push" // backups pushed to S3 / WebDAV / Telegram
)

// AllFeatures lists every paid feature (what a trial unlocks).
var AllFeatures = []string{FeatureSpeedLimit, FeatureTGMenu, FeatureBackupPush}

// Limit kinds.
const (
	LimitNodes = "nodes"
	LimitUsers = "users"
)

// Community edition: no paid features; node and user caps are not set yet
// (0 = unlimited).
var Community = Limits{Nodes: 0, Users: 0}

// Limits caps what an instance may manage (0 = unlimited).
type Limits struct {
	Nodes int `json:"nodes"`
	Users int `json:"users"`
}

// License is the signed document.
type License struct {
	ID       string   `json:"id"`
	Customer string   `json:"customer"`
	Email    string   `json:"email,omitempty"`
	Plan     string   `json:"plan"` // pro / enterprise
	Limits   Limits   `json:"limits"`
	Features []string `json:"features"`
	// Instance binds the license to one panel ("" = any panel).
	Instance string `json:"instance,omitempty"`
	// Offline licenses never contact the license server.
	Offline bool `json:"offline,omitempty"`
	// Dates are YYYY-MM-DD; empty ExpiresAt = perpetual, empty
	// UpdatesUntil = every version.
	IssuedAt     string `json:"issued_at"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	UpdatesUntil string `json:"updates_until,omitempty"`
}

const dateLayout = "2006-01-02"

func day(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(dateLayout, s)
	return t, err == nil
}

// Expired reports whether the license ran out before now (the expiry day
// itself still counts).
func (l *License) Expired(now time.Time) bool {
	t, ok := day(l.ExpiresAt)
	return ok && now.After(t.Add(24*time.Hour))
}

// CoversRelease reports whether a version released on date may run.
func (l *License) CoversRelease(released time.Time) bool {
	t, ok := day(l.UpdatesUntil)
	return !ok || released.IsZero() || !released.After(t.Add(24*time.Hour))
}

// Has reports whether the license includes a feature.
func (l *License) Has(f string) bool { return slices.Contains(l.Features, f) }

var b64 = base64.RawURLEncoding

// Sign encodes and signs a license (used by the license server and tests).
func Sign(l *License, key ed25519.PrivateKey) (string, error) {
	body, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(body) + "." + b64.EncodeToString(ed25519.Sign(key, body)), nil
}

// Errors returned by Parse.
var (
	ErrFormat    = errors.New("许可证格式不正确")
	ErrSignature = errors.New("许可证签名无效（不是官方签发，或内容被修改）")
	ErrInstance  = errors.New("许可证绑定的是另一个面板实例")
)

// Parse verifies a license against any of the public keys and, when the
// license is bound to an instance, against this instance.
func Parse(text string, keys []ed25519.PublicKey, instance string) (*License, error) {
	text = strings.Join(strings.Fields(text), "") // tolerate line breaks from copy and paste
	body64, sig64, ok := strings.Cut(text, ".")
	if !ok {
		return nil, ErrFormat
	}
	body, err1 := b64.DecodeString(body64)
	sig, err2 := b64.DecodeString(sig64)
	if err1 != nil || err2 != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrFormat
	}
	valid := false
	for _, k := range keys {
		if ed25519.Verify(k, body, sig) {
			valid = true
			break
		}
	}
	if !valid {
		return nil, ErrSignature
	}
	var l License
	if err := json.Unmarshal(body, &l); err != nil {
		return nil, ErrFormat
	}
	if l.ID == "" {
		return nil, fmt.Errorf("%w：缺少编号", ErrFormat)
	}
	for _, d := range []string{l.IssuedAt, l.ExpiresAt, l.UpdatesUntil} {
		if _, ok := day(d); d != "" && !ok {
			return nil, fmt.Errorf("%w：日期 %q", ErrFormat, d)
		}
	}
	if l.Instance != "" && l.Instance != instance {
		return nil, ErrInstance
	}
	return &l, nil
}

// ParseKeys reads comma-separated base64 (std or URL) Ed25519 public keys.
func ParseKeys(s string) []ed25519.PublicKey {
	var out []ed25519.PublicKey
	for _, k := range strings.Split(s, ",") {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(k)
		if err != nil {
			b, err = b64.DecodeString(k)
		}
		if err == nil && len(b) == ed25519.PublicKeySize {
			out = append(out, ed25519.PublicKey(b))
		}
	}
	return out
}

// PublicKeys are the license server's keys, set at build time with
// -ldflags "-X github.com/cuijianzhuang/polaris/internal/license.PublicKeys=<b64>,<b64>"
// (two keys allow rotating one). Development builds have none and accept no
// license.
var PublicKeys = ""
