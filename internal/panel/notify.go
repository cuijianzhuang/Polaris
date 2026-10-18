package panel

// Event is something worth telling operators (and sometimes users) about.
// Delivery is pluggable; the Telegram bot is the built-in sink.
type Event struct {
	Kind   string // node_offline, node_online, apply_failed, user_limited, user_expired, ip_limit, ip_changed, ddns_failed, cert_failed, traffic_warn, expire_warn
	NodeID string // scopes the event to node operators bound to this node
	UserID string // also delivered to the user's bound chat, if any
	Text   string
	// UserOnly skips admins (per-user reminders; admins get the daily report).
	UserOnly bool
	// Type is the dotted name for webhooks ("user.expired"); derived from
	// Kind when empty. Data adds structured fields to the webhook payload.
	Type string
	Data map[string]any
}

// notify fans an event out to every registered sink without blocking.
func (s *Server) notify(e Event) {
	for _, sink := range s.sinks {
		select {
		case sink <- e:
		default: // drop rather than stall the control plane
		}
	}
}
