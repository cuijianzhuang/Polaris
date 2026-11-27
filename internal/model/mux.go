package model

// MuxFeature prevents old agents from silently ignoring SNI mux groups:
// a spec carrying Muxes needs an agent that runs the built-in L4 splitter.
const MuxFeature = "sni-mux-v1"

// Mux fallback kinds, as stored on the panel's mux group.
const (
	MuxFallbackNone     = ""         // unmatched traffic is dropped (connection closed)
	MuxFallbackMember   = "member"   // unmatched traffic goes to one member inbound
	MuxFallbackExternal = "external" // unmatched traffic goes to an outside host:port
)

// MuxRoute is one SNI -> loopback upstream mapping inside a MuxSpec.
type MuxRoute struct {
	SNI      string `json:"sni"`
	Upstream string `json:"upstream"` // 127.0.0.1:port of the member inbound
	Inbound  string `json:"inbound"`  // member tag, for messages
}

// MuxSpec is one SNI mux group as dispatched to the agent: a public TCP
// listener whose connections are passed through, unmodified and without
// terminating TLS, to the member inbound matching the ClientHello SNI.
// The splitter holds no private keys; members keep their own certificates
// and terminate TLS on their loopback ports.
type MuxSpec struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Listen is the bind address ("" = all interfaces).
	Listen string `json:"listen,omitempty"`
	Port   int    `json:"port"`

	Routes []MuxRoute `json:"routes"`
	// Fallback is the resolved upstream (host:port) for traffic whose SNI
	// matches no route — a member's loopback address or an external site.
	// "" drops the connection.
	Fallback string `json:"fallback,omitempty"`
	// FallbackInbound is the member tag when Fallback is a member.
	FallbackInbound string `json:"fallbackInbound,omitempty"`
}
