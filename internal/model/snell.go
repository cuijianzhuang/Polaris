package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SnellUsersFeature prevents older Agents from applying an unmetered or
// shared-credential interpretation of a Snell listener.
const SnellUsersFeature = "snell-users-v1"
const SnellMinVersion = "1.14.0"

func (in *Inbound) ValidateSnell() error {
	p := in.Protocol
	if in.Core != "" && in.Core != CoreAuto && in.Core != CoreSingbox {
		return fmt.Errorf("Snell multi-user requires sing-box >= %s", SnellMinVersion)
	}
	if (in.Transport.Type != "" && in.Transport.Type != TransTCP) || (in.Security.Type != "" && in.Security.Type != SecNone) {
		return fmt.Errorf("Snell requires TCP without an additional TLS/REALITY layer")
	}
	if p.SnellVersion != 0 && p.SnellVersion != 5 && p.SnellVersion != 6 {
		return fmt.Errorf("Snell server version must be 5 or 6")
	}
	if p.SnellVersion == 6 {
		if p.SnellObfs != "" && p.SnellObfs != "none" {
			return fmt.Errorf("Snell v6 does not support HTTP obfuscation")
		}
		switch p.SnellMode {
		case "", "default", "unshaped", "unsafe-raw":
		default:
			return fmt.Errorf("invalid Snell v6 mode")
		}
	} else {
		if p.SnellMode != "" {
			return fmt.Errorf("Snell v5 does not support traffic shaping mode")
		}
		if p.SnellObfs != "" && p.SnellObfs != "none" && p.SnellObfs != "http" {
			return fmt.Errorf("invalid Snell v5 obfuscation")
		}
	}
	if p.ServerKey != "" && (len(p.ServerKey) < 12 || len(p.ServerKey) > 255) {
		return fmt.Errorf("Snell server PSK must contain 12–255 bytes")
	}
	// Native overrides must not bypass authentication, user identity or accounting.
	if len(in.Extra) > 0 {
		var extra map[string]any
		if err := json.Unmarshal(in.Extra, &extra); err != nil {
			return err
		}
		for key := range extra {
			switch strings.ToLower(key) {
			case "type", "tag", "users", "psk", "version", "mode", "obfs_mode", "detour":
				return fmt.Errorf("Snell extra cannot override %s", key)
			}
		}
	}
	return nil
}
