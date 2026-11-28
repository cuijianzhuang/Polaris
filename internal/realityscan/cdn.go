package realityscan

import (
	"context"
	"net"
	"net/netip"
	"strings"
)

// A CDN-fronted site is a poor REALITY target: the borrowed handshake and
// certificate belong to a shared anycast edge whose configuration the site
// owner does not control and which can change at any time. Probing therefore
// reports the CDN behind a target and refuses to call it usable.
//
// Detection is two-pronged, from the probing side's point of view:
//   - the dial name or its canonical (CNAME) name ends in a known CDN
//     hostname suffix — this is how Akamai is caught, as it publishes no
//     address list;
//   - the address actually connected to falls in a provider's published
//     address space (cdn_prefixes.go) — this catches names pointed straight
//     at CDN space with an A/AAAA record or a Route53-style alias, where no
//     CNAME is visible at all (e.g. a name resolving directly to Cloudflare
//     or CloudFront addresses).

// cdnSuffixes maps CDN-owned hostname suffixes to provider names.
var cdnSuffixes = []struct{ suffix, provider string }{
	{"cloudflare.net", "Cloudflare"},
	{"cloudfront.net", "CloudFront"},
	{"akamai.net", "Akamai"},
	{"akamaiedge.net", "Akamai"},
	{"akamaized.net", "Akamai"},
	{"edgesuite.net", "Akamai"},
	{"edgekey.net", "Akamai"},
	{"fastly.net", "Fastly"},
	{"fastlylb.net", "Fastly"},
	{"kxcdn.com", "KeyCDN"},
	{"stackpathcdn.com", "StackPath"},
	{"incapdns.net", "Imperva"},
	{"b-cdn.net", "BunnyCDN"},
	{"cdn77.org", "CDN77"},
	{"azureedge.net", "Azure CDN"},
	{"gcdn.co", "Gcore"},
}

type cdnNet struct {
	provider string
	prefix   netip.Prefix
}

var cdnNets []cdnNet

func init() {
	for _, src := range []struct {
		provider string
		prefixes []string
	}{{"Cloudflare", cloudflarePrefixes}, {"Fastly", fastlyPrefixes}, {"CloudFront", cloudfrontPrefixes}} {
		for _, s := range src.prefixes {
			if p, err := netip.ParsePrefix(s); err == nil {
				cdnNets = append(cdnNets, cdnNet{src.provider, p.Masked()})
			}
		}
	}
}

// lookupCNAME resolves a name's canonical name; tests replace it.
var lookupCNAME = func(ctx context.Context, name string) (string, error) {
	return net.DefaultResolver.LookupCNAME(ctx, name)
}

// cdnCheck is the probe's CDN test; tests replace it.
var cdnCheck = CDNProvider

// CDNProvider reports the CDN in front of a target, or "" when the peer
// looks like a direct origin. host is the dial name (an IP literal skips
// the name checks); peer is the address actually connected to.
func CDNProvider(ctx context.Context, host string, peer netip.Addr) string {
	if p := cdnByName(host); p != "" {
		return p
	}
	if host != "" && net.ParseIP(host) == nil {
		if canonical, err := lookupCNAME(ctx, host); err == nil {
			if p := cdnByName(canonical); p != "" {
				return p
			}
		}
	}
	if peer.IsValid() {
		return cdnByIP(peer)
	}
	return ""
}

func cdnByName(name string) string {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if name == "" {
		return ""
	}
	for _, s := range cdnSuffixes {
		if name == s.suffix || strings.HasSuffix(name, "."+s.suffix) {
			return s.provider
		}
	}
	return ""
}

func cdnByIP(ip netip.Addr) string {
	ip = ip.Unmap()
	for _, n := range cdnNets {
		if n.prefix.Contains(ip) {
			return n.provider
		}
	}
	return ""
}
