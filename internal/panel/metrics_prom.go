package panel

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Prometheus metrics.
//
// GET {admin}/api/v1/metrics answers in the Prometheus text format (0.0.4),
// for an admin session or an API token with the metrics:read scope. Scrape
// it with a bearer token:
//
//	scrape_configs:
//	  - job_name: polaris
//	    scheme: https
//	    metrics_path: /<admin path>/api/v1/metrics
//	    authorization: {credentials: plt_…}
//	    static_configs: [{targets: [panel.example.com]}]
//
// Everything is read from the database at scrape time; nothing is kept in
// memory between scrapes, so every panel instance answers the same. Labels
// stay bounded: one series per node, profile or status, never per user.

const ScopeMetricsRead = "metrics:read"

type promWriter struct {
	b    strings.Builder
	seen map[string]bool
}

func (w *promWriter) head(name, typ, help string) {
	if w.seen == nil {
		w.seen = map[string]bool{}
	}
	if w.seen[name] {
		return
	}
	w.seen[name] = true
	fmt.Fprintf(&w.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// sample writes one series; labels alternate name, value.
func (w *promWriter) sample(name string, v float64, labels ...string) {
	w.b.WriteString(name)
	if len(labels) > 0 {
		w.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.b.WriteByte(',')
			}
			fmt.Fprintf(&w.b, "%s=\"%s\"", labels[i], promEscape(labels[i+1]))
		}
		w.b.WriteByte('}')
	}
	w.b.WriteByte(' ')
	switch {
	case math.IsNaN(v):
		w.b.WriteString("NaN")
	case math.IsInf(v, 1):
		w.b.WriteString("+Inf")
	case math.IsInf(v, -1):
		w.b.WriteString("-Inf")
	default:
		w.b.WriteString(fmt.Sprintf("%g", v))
	}
	w.b.WriteByte('\n')
}

func (w *promWriter) gauge(name, help string, v float64, labels ...string) {
	w.head(name, "gauge", help)
	w.sample(name, v, labels...)
}

func promEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// countBy counts rows of a table grouped by one column.
func (s *Server) countBy(model any, col string, where string, args ...any) map[string]int64 {
	type row struct {
		K string
		N int64
	}
	var rows []row
	q := s.st.DB.Model(model).Select(col + " AS k, COUNT(*) AS n").Group(col)
	if where != "" {
		q = q.Where(where, args...)
	}
	q.Scan(&rows)
	out := map[string]int64{}
	for _, r := range rows {
		out[r.K] = r.N
	}
	return out
}

func (s *Server) prometheusMetrics(c *gin.Context) {
	now := time.Now()
	w := &promWriter{}
	w.gauge("polaris_build_info", "Panel version.", 1, "version", Version)
	w.gauge("polaris_scrape_timestamp_seconds", "When this scrape was served.", float64(now.Unix()))

	// Nodes.
	var nodes []store.Node
	s.st.DB.Where("revoked = ?", false).Order("name").Find(&nodes)
	status := map[string]int64{}
	for _, n := range nodes {
		status[n.Status]++
	}
	w.head("polaris_nodes", "gauge", "Nodes by status.")
	for _, k := range []string{"online", "offline", "pending", "syncing", "error"} {
		w.sample("polaris_nodes", float64(status[k]), "status", k)
	}
	for _, n := range nodes {
		l := []string{"node", n.ID, "name", n.Name}
		w.gauge("polaris_node_up", "1 when the node's agent is connected.", boolF(n.Status == "online" || n.Status == "syncing"), l...)
	}
	for _, n := range nodes {
		if n.LastSeen != nil {
			w.gauge("polaris_node_last_seen_seconds", "Unix time of the node's last heartbeat.", float64(n.LastSeen.Unix()), "node", n.ID, "name", n.Name)
		}
	}
	for _, n := range nodes {
		if n.Status != "online" && n.Status != "syncing" {
			continue // a stale snapshot is not a measurement
		}
		sys, l := n.System.V, []string{"node", n.ID, "name", n.Name}
		w.gauge("polaris_node_cpu_percent", "CPU usage of online nodes.", sys.CPU, l...)
		w.gauge("polaris_node_memory_percent", "Memory usage of online nodes.", sys.Mem, l...)
		if sys.DiskTotal > 0 {
			w.gauge("polaris_node_disk_percent", "Disk usage of online nodes.", sys.Disk, l...)
		}
		w.gauge("polaris_node_load1", "1-minute load average of online nodes.", sys.Load1, l...)
		w.gauge("polaris_node_network_receive_bytes_per_second", "Inbound NIC rate of online nodes.", float64(sys.RxBps), l...)
		w.gauge("polaris_node_network_transmit_bytes_per_second", "Outbound NIC rate of online nodes.", float64(sys.TxBps), l...)
	}
	for _, n := range nodes {
		l := []string{"node", n.ID, "name", n.Name}
		w.gauge("polaris_node_cycle_receive_bytes", "NIC bytes received this billing cycle.", float64(n.NetRx), l...)
	}
	for _, n := range nodes {
		l := []string{"node", n.ID, "name", n.Name}
		w.gauge("polaris_node_cycle_transmit_bytes", "NIC bytes sent this billing cycle.", float64(n.NetTx), l...)
	}

	// Users: counts and total usage, never per-user series.
	users := s.countBy(&store.User{}, "status", "is_service = ?", false)
	w.head("polaris_users", "gauge", "User accounts by status.")
	for _, k := range sortedKeys(users) {
		w.sample("polaris_users", float64(users[k]), "status", k)
	}
	var used struct{ Up, Down int64 }
	s.st.DB.Model(&store.User{}).Select("COALESCE(SUM(used_up),0) AS up, COALESCE(SUM(used_down),0) AS down").Where("is_service = ?", false).Scan(&used)
	w.gauge("polaris_users_traffic_used_bytes", "Traffic all users used this cycle (after node rates).", float64(used.Up), "direction", "up")
	w.sample("polaris_users_traffic_used_bytes", float64(used.Down), "direction", "down")
	var expiring int64
	s.st.DB.Model(&store.User{}).Where("is_service = ? AND status = ? AND expire_at IS NOT NULL AND expire_at < ?", false, "active", now.Add(7*24*time.Hour)).Count(&expiring)
	w.gauge("polaris_users_expiring_7d", "Active users whose account expires within 7 days.", float64(expiring))

	// Orders and tickets.
	orders := s.countBy(&store.Order{}, "status", "")
	w.head("polaris_orders", "gauge", "Shop orders by status.")
	for _, k := range sortedKeys(orders) {
		w.sample("polaris_orders", float64(orders[k]), "status", k)
	}
	var paid24 int64
	s.st.DB.Model(&store.Order{}).Where("paid_at > ?", now.Add(-24*time.Hour)).Count(&paid24)
	w.gauge("polaris_orders_paid_24h", "Orders paid in the last 24 hours.", float64(paid24))
	tickets := s.countBy(&store.Ticket{}, "status", "")
	w.head("polaris_tickets", "gauge", "Support tickets by status.")
	for _, k := range sortedKeys(tickets) {
		w.sample("polaris_tickets", float64(tickets[k]), "status", k)
	}

	// Certificates and the license.
	var certs []store.Certificate
	s.st.DB.Where("expire_at IS NOT NULL").Order("domain").Find(&certs)
	for _, ct := range certs {
		w.gauge("polaris_certificate_expiry_seconds", "Unix time each certificate expires.", float64(ct.ExpireAt.Unix()), "domain", ct.Domain)
	}
	lic := s.license()
	w.gauge("polaris_license_info", "License plan and source.", 1, "plan", lic.Plan, "source", lic.Source)
	if t, err := time.Parse("2006-01-02", lic.ExpiresAt); err == nil {
		w.gauge("polaris_license_expiry_seconds", "Unix time the license expires (absent when perpetual).", float64(t.Unix()))
	} else if lic.TrialUntil != nil {
		w.gauge("polaris_license_expiry_seconds", "Unix time the license expires (absent when perpetual).", float64(lic.TrialUntil.Unix()))
	}

	// CDN preferred IPs.
	var profiles []store.CDNProfile
	s.st.DB.Order("name").Find(&profiles)
	for _, p := range profiles {
		w.gauge("polaris_cdn_preferred_ips", "Preferred IPs in each CDN profile.", float64(len(p.PreferredIPs.V)), "profile", p.Name)
	}
	for _, p := range profiles {
		if p.NodeCheck {
			w.gauge("polaris_cdn_unreachable_ips", "Preferred IPs no node could reach in the last check.", float64(len(p.BadIPs.V)), "profile", p.Name)
		}
	}
	names := map[string]string{}
	for _, p := range profiles {
		names[p.ID] = p.Name
	}
	var reports []store.CDNIPReport
	s.st.DB.Order("profile_id, source").Find(&reports)
	for _, r := range reports {
		w.gauge("polaris_cdn_report_age_seconds", "Seconds since each speed-test point last reported.", now.Sub(r.At).Seconds(), "profile", names[r.ProfileID], "source", r.Source)
	}

	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(w.b.String()))
}
