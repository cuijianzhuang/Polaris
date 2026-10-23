package model

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strings"
)

// OutboundIPStrategy controls sockets leaving the node, independently of the
// inbound listener and the address advertised to clients.
type OutboundIPStrategy string

const (
	OutboundIPAuto     OutboundIPStrategy = ""
	OutboundIPv4Only   OutboundIPStrategy = "ipv4_only"
	OutboundIPv6Only   OutboundIPStrategy = "ipv6_only"
	OutboundPreferIPv4 OutboundIPStrategy = "prefer_ipv4"
	OutboundPreferIPv6 OutboundIPStrategy = "prefer_ipv6"
)

func (s OutboundIPStrategy) Validate() error {
	switch s {
	case OutboundIPAuto, "auto", OutboundIPv4Only, OutboundIPv6Only, OutboundPreferIPv4, OutboundPreferIPv6:
		return nil
	}
	return fmt.Errorf("unknown outbound IP strategy %q", s)
}
func (s OutboundIPStrategy) IsAuto() bool { return s == "" || s == "auto" }
func (s OutboundIPStrategy) Strict() bool { return s == OutboundIPv4Only || s == OutboundIPv6Only }
func (s OutboundIPStrategy) IPv6() bool   { return s == OutboundIPv6Only || s == OutboundPreferIPv6 }

const OutboundIPFeature = "outbound-ip-v1"
const LimitScopeSeparator = "#ip:"

// ScopedLimitUser keeps the original inbound through the limiter's SOCKS
// round trip. It is an internal transport identity, never a billable user.
func ScopedLimitUser(user, inbound string) string {
	sum := sha256.Sum256([]byte(inbound))
	return fmt.Sprintf("%s%s%x", user, LimitScopeSeparator, sum[:8])
}

// LimitBaseUser shares a user's token buckets across all inbound contexts.
func LimitBaseUser(user string) string {
	base, scope, ok := strings.Cut(user, LimitScopeSeparator)
	if !ok || base == "" || len(scope) != 16 {
		return user
	}
	for _, r := range scope {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return user
		}
	}
	return base
}

// LimitUUID authenticates an internal VLESS re-entry without reusing a public
// user's credentials. Both the limiter and renderer derive the same ID.
func LimitUUID(secret, user string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte("polaris-limiter-v1\x00" + user))
	b := h.Sum(nil)[:16]
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
