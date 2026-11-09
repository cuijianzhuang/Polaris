package model

// AgentFeatures is the canonical, ordered list of feature flags an Agent may
// advertise through the X-Polaris-Agent-Features header. The panel accepts only
// these names, and the agent builds its advertised set from this table, so the
// allowlist and the advertisement cannot drift. Add a new feature's constant
// here (and gate it in the agent's featureSet when needed).
var AgentFeatures = []string{
	WebsiteFeature,
	RelayAccessFeature,
	OutboundIPFeature,
	SnellUsersFeature,
	SnellPSKFeature,
	AgentRollbackFeature,
	RouteTraceFeature,
	RouteTraceToolFeature,
	ManagedTunnelFeature,
	RemoteTunnelFeature,
	RelayBusinessFeature,
	SingboxRelayBusinessFeature,
	ExtendedRelayBusinessFeature,
}

// IsAgentFeature reports whether name is a known agent feature flag.
func IsAgentFeature(name string) bool {
	for _, f := range AgentFeatures {
		if f == name {
			return true
		}
	}
	return false
}
