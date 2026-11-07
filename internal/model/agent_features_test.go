package model

import "testing"

func TestAgentFeaturesRegistered(t *testing.T) {
	// Every feature constant must be present in AgentFeatures; forgetting to
	// append one here would make the panel silently reject a feature the agent
	// advertises.
	want := []string{
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
	}
	for _, w := range want {
		if !IsAgentFeature(w) {
			t.Errorf("feature %q missing from AgentFeatures", w)
		}
	}
	for _, f := range AgentFeatures {
		if !IsAgentFeature(f) {
			t.Errorf("IsAgentFeature(%q) = false", f)
		}
	}
	if IsAgentFeature("not-a-feature") {
		t.Error("IsAgentFeature accepted an unknown feature")
	}
}
