package model

import "fmt"

// Combo is one protocol × transport × security combination.
type Combo struct {
	Protocol  Protocol  `json:"protocol"`
	Transport Transport `json:"transport"`
	Security  Security  `json:"security"`
}

func (c Combo) String() string { return fmt.Sprintf("%s+%s+%s", c.Protocol, c.Transport, c.Security) }

// CapSet is what one core version can serve. Adapters compute it from their
// version, so the matrix is never hard-coded in the panel or frontend.
type CapSet struct {
	Core    CoreType `json:"core"`
	Version string   `json:"version"`
	Combos  []Combo  `json:"combos"`
	// Flows lists supported VLESS flows.
	Flows []string `json:"flows,omitempty"`
	// Features lists optional inbound features (Feature* constants).
	Features []string `json:"features,omitempty"`
	// HotUsers reports whether users can be added/removed without a reload.
	HotUsers bool `json:"hotUsers"`
	// Outbounds lists supported outbound types (for chain validation).
	Outbounds []OutboundType `json:"outbounds,omitempty"`
	// RealIP reports whether the core can read CF-Connecting-IP / X-Forwarded-For.
	RealIP bool `json:"realIp"`
	// Balancers lists supported load-balancing strategies.
	Balancers []string `json:"balancers,omitempty"`
}

// SupportsBalancer reports whether a load-balancing strategy is available.
func (c CapSet) SupportsBalancer(strategy string) bool {
	for _, s := range c.Balancers {
		if s == strategy {
			return true
		}
	}
	return false
}

// Supports reports whether the combination is available.
func (c CapSet) Supports(p Protocol, t Transport, s Security) bool {
	for _, cb := range c.Combos {
		if cb.Protocol == p && cb.Transport == t && cb.Security == s {
			return true
		}
	}
	return false
}

// SupportsInbound checks an inbound against the set.
func (c CapSet) SupportsInbound(in *Inbound) error {
	if !c.Supports(in.Protocol.Type, in.Transport.Type, in.Security.Type) {
		return fmt.Errorf("%s %s does not support %s+%s+%s", c.Core, c.Version,
			in.Protocol.Type, in.Transport.Type, in.Security.Type)
	}
	need := func(f, what string) error {
		for _, x := range c.Features {
			if x == f {
				return nil
			}
		}
		return fmt.Errorf("%s %s does not support %s", c.Core, c.Version, what)
	}
	if in.Protocol.Decryption != "" && in.Protocol.Decryption != "none" {
		if err := need(FeatureVLESSEncryption, "VLESS Encryption"); err != nil {
			return err
		}
	}
	if in.Security.RealityMldsa65Seed != "" {
		if err := need(FeatureRealityMLDSA65, "REALITY ML-DSA-65 signatures"); err != nil {
			return err
		}
	}
	if in.Transport.AcceptProxyProtocol {
		if err := need(FeatureProxyProtocol, "PROXY protocol"); err != nil {
			return err
		}
	}
	if in.Protocol.Flow != "" {
		ok := false
		for _, f := range c.Flows {
			if f == in.Protocol.Flow {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("%s does not support flow %q", c.Core, in.Protocol.Flow)
		}
	}
	return nil
}

// SupportsProtocol reports whether any combination with the protocol exists.
func (c CapSet) SupportsProtocol(p Protocol) bool {
	for _, cb := range c.Combos {
		if cb.Protocol == p {
			return true
		}
	}
	return false
}

// SupportsOutbound reports whether the outbound type is renderable.
func (c CapSet) SupportsOutbound(t OutboundType) bool {
	for _, o := range c.Outbounds {
		if o == t {
			return true
		}
	}
	return false
}

// CapBuilder helps adapters build a combo list from cartesian products.
type CapBuilder struct{ combos []Combo }

func (b *CapBuilder) Add(ps []Protocol, ts []Transport, ss []Security) *CapBuilder {
	for _, p := range ps {
		for _, t := range ts {
			for _, s := range ss {
				b.combos = append(b.combos, Combo{p, t, s})
			}
		}
	}
	return b
}

func (b *CapBuilder) Combos() []Combo { return b.combos }

// Optional inbound features a core may list in CapSet.Features.
const (
	FeatureVLESSEncryption = "vless-encryption"
	FeatureRealityMLDSA65  = "reality-mldsa65"
	FeatureProxyProtocol   = "proxy-protocol"
)
