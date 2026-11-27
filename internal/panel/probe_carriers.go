package panel

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

// Three-carrier ("backhaul") view, design docs/design/backhaul-monitor.md
// batch 1: aggregate the latency the TCP probe already collects by the
// target's carrier (ct/cu/cm). No new tables, no agent changes, no line
// classification (batch 2 fills the line fields, null until then).

// carrierFreshWindow is how old the newest probe sample may be before the
// "current" figures stop being presented as current (the probe's 90-second
// freshness convention). Older data remains visible as 1h/24h history.
const carrierFreshWindow = 90 * time.Second

var carrierKeys = []string{"ct", "cu", "cm"}

// carrierCurrent is one fresh reading: the last probe's latency plus the
// latest 5-minute slot's average and connection failure rate.
type carrierCurrent struct {
	Ms   float64   `json:"ms"`   // last probe, 0 = it failed
	Avg  *float64  `json:"avg"`  // latest slot average, nil when every attempt failed
	Loss *float64  `json:"loss"` // latest slot, percent; nil when the slot has no samples
	At   time.Time `json:"at"`
}

// carrierTrendPoint is one 5-minute slot of a carrier's 24h trend,
// aggregated across the carrier's targets weighted by sample count.
// Avg/Loss are nil for slots without (successful) samples: gaps stay gaps.
type carrierTrendPoint struct {
	T    time.Time `json:"t"`
	Avg  *float64  `json:"avg"`
	Loss *float64  `json:"loss"`
}

type carrierTargetView struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	State   string          `json:"state"` // ok / stale / no_data
	Current *carrierCurrent `json:"current"`
	HourAvg *float64        `json:"hourAvg"`
	Loss1h  *float64        `json:"loss1h"` // percent
}

// carrierView is one carrier's column of the three-carrier section.
// State: ok / stale / no_data / no_target / disabled. Line fields stay
// null in batch 1; batch 2 (route classification) fills them.
type carrierView struct {
	State      string              `json:"state"`
	Current    *carrierCurrent     `json:"current"`
	HourAvg    *float64            `json:"hourAvg"`
	Loss1h     *float64            `json:"loss1h"`
	Line       *string             `json:"line"`
	Confidence *string             `json:"confidence"`
	LineAt     *time.Time          `json:"lineAt"`
	Trend      []carrierTrendPoint `json:"trend"`
	Targets    []carrierTargetView `json:"targets"`
}

// probeTargetV6 reports whether a target is IPv6-only. The three-carrier
// aggregation is IPv4-only in this batch (design D3): an IPv6 literal
// address, or a -v6 host label (the zstatic naming convention), marks a
// v6 target; everything else counts as reachable over IPv4.
func probeTargetV6(t store.ProbeTarget) bool {
	host, _, err := net.SplitHostPort(t.Addr)
	if err != nil {
		host = t.Addr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4() == nil
	}
	h := strings.ToLower(host)
	return strings.Contains(h, "-v6.") || strings.HasSuffix(h, "-v6")
}

// slotAcc folds NodeLatency rows of one (set of) target(s) into weighted
// figures, the same weighting as pointOf/latencySummary: the average is
// SumMs over successful attempts, the loss is Lost over Sent.
type slotAcc struct {
	sent, lost int
	sum        float64
}

func (a *slotAcc) add(m *store.NodeLatency) {
	a.sent += m.Sent
	a.lost += m.Lost
	a.sum += m.SumMs
}

func (a *slotAcc) avg() *float64 {
	if ok := a.sent - a.lost; ok > 0 {
		v := a.sum / float64(ok)
		return &v
	}
	return nil
}

func (a *slotAcc) loss() *float64 {
	if a.sent > 0 {
		v := 100 * float64(a.lost) / float64(a.sent)
		return &v
	}
	return nil
}

// carrierSummary builds the three-carrier view for one node from the last
// 24 hours of NodeLatency rows of the node's effective probe targets.
func (s *Server) carrierSummary(n *store.Node, now time.Time) gin.H {
	targets := nodeProbeTargets(n, s.latencyTargets(true))
	byCarrier := map[string][]store.ProbeTarget{}
	for _, t := range targets {
		if !probeCarriers[t.Carrier] || t.Carrier == "" || probeTargetV6(t) {
			continue
		}
		byCarrier[t.Carrier] = append(byCarrier[t.Carrier], t)
	}
	// Probing can be off globally or per node (policy mode off / unknown
	// fails closed in nodeProbeTargets); the columns then say so instead
	// of dressing old numbers up as current.
	disabled := !s.probeEnabled() || (n.ProbeMode != "" && n.ProbeMode != "inherit" && n.ProbeMode != "custom")

	ids := []string{}
	for _, ts := range byCarrier {
		for _, t := range ts {
			ids = append(ids, t.ID)
		}
	}
	from := now.UTC().Add(-24 * time.Hour).Truncate(latencySlot)
	var rows []store.NodeLatency
	if len(ids) > 0 {
		s.st.DB.Where("node_id = ? AND target_id IN ? AND at >= ?", n.ID, ids, from).Order("at").Find(&rows)
	}
	byTarget := map[string][]*store.NodeLatency{}
	for i := range rows {
		byTarget[rows[i].TargetID] = append(byTarget[rows[i].TargetID], &rows[i])
	}

	const slots = int(24*time.Hour/latencySlot) + 1
	out := gin.H{}
	for _, ck := range carrierKeys {
		ts := byCarrier[ck]
		v := carrierView{Trend: make([]carrierTrendPoint, 0, slots), Targets: []carrierTargetView{}}
		switch {
		case disabled:
			v.State = "disabled"
		case len(ts) == 0:
			v.State = "no_target"
		}
		slotAccs := make([]slotAcc, slots)
		var hourAcc, allAcc slotAcc
		var freshest time.Time
		for _, t := range ts {
			tv := carrierTargetView{ID: t.ID, Name: t.Name, State: "no_data"}
			var tHour slotAcc
			var latestSlot *store.NodeLatency
			var latestSample *store.NodeLatency
			for _, m := range byTarget[t.ID] {
				if idx := int(m.At.Sub(from) / latencySlot); idx >= 0 && idx < slots {
					slotAccs[idx].add(m)
				}
				if m.At.After(now.Add(-time.Hour)) {
					tHour.add(m)
					hourAcc.add(m)
				}
				allAcc.add(m)
				if latestSlot == nil || m.At.After(latestSlot.At) {
					latestSlot = m
				}
				if latestSample == nil || m.LastAt.After(latestSample.LastAt) {
					latestSample = m
				}
			}
			if latestSample != nil {
				if latestSample.LastAt.After(freshest) {
					freshest = latestSample.LastAt
				}
				tv.HourAvg, tv.Loss1h = tHour.avg(), tHour.loss()
				if now.Sub(latestSample.LastAt) <= carrierFreshWindow && !latestSample.LastAt.After(now.Add(time.Minute)) {
					sa := slotAcc{}
					sa.add(latestSlot)
					tv.State = "ok"
					tv.Current = &carrierCurrent{Ms: latestSample.LastMs, Avg: sa.avg(), Loss: sa.loss(), At: latestSample.LastAt}
				} else if len(byTarget[t.ID]) > 0 {
					tv.State = "stale"
				}
			}
			v.Targets = append(v.Targets, tv)
		}
		for i := range slotAccs {
			p := carrierTrendPoint{T: from.Add(time.Duration(i) * latencySlot)}
			if slotAccs[i].sent > 0 {
				p.Avg, p.Loss = slotAccs[i].avg(), slotAccs[i].loss()
			}
			v.Trend = append(v.Trend, p)
		}
		if v.State == "" {
			switch {
			case allAcc.sent == 0:
				v.State = "no_data"
			case !freshest.IsZero() && now.Sub(freshest) <= carrierFreshWindow && !freshest.After(now.Add(time.Minute)):
				v.State = "ok"
				v.HourAvg, v.Loss1h = hourAcc.avg(), hourAcc.loss()
				// Carrier "current": the freshest sample's latency plus the
				// figures of the most recent slot, aggregated across the
				// carrier's targets that reported into that slot.
				var latestAt time.Time
				for _, t := range ts {
					for _, m := range byTarget[t.ID] {
						if m.At.After(latestAt) {
							latestAt = m.At
						}
					}
				}
				var curAcc slotAcc
				var curSample *store.NodeLatency
				for _, t := range ts {
					for _, m := range byTarget[t.ID] {
						if m.At.Equal(latestAt) {
							curAcc.add(m)
						}
						if curSample == nil || m.LastAt.After(curSample.LastAt) {
							curSample = m
						}
					}
				}
				v.Current = &carrierCurrent{Ms: curSample.LastMs, Avg: curAcc.avg(), Loss: curAcc.loss(), At: curSample.LastAt}
			default:
				v.State = "stale"
				v.HourAvg, v.Loss1h = hourAcc.avg(), hourAcc.loss()
			}
		}
		out[ck] = v
	}
	return gin.H{"probeEnabled": s.probeEnabled(), "slotSeconds": int(latencySlot.Seconds()), "carriers": out}
}

// nodeCarriers: GET /nodes/:id/carriers — the three-carrier aggregate for
// the node detail page. Members/operators reach it through the usual node
// visibility (route whitelists + node scope), like the other node reads.
func (s *Server) nodeCarriers(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	c.JSON(http.StatusOK, s.carrierSummary(&n, time.Now()))
}

// --- public projection (backhaul design D10) ---
//
// The shape is fixed by docs/design/backhaul-monitor.md D10 and shared by
// every public surface: the service snapshot below and the monitor
// snapshot when backhaul batch 3 wires it in. One projection, so the two
// can never drift apart. Only aggregate numbers and the line label leave
// the panel — never a target address, route or evidence detail.

// serviceCarrier is one carrier's public figures. A carrier without data
// stays present but all-null, keeping the {ct, cu, cm} shape fixed.
type serviceCarrier struct {
	Ms   *float64 `json:"ms"`   // last probe; nil = no live sample
	Loss *float64 `json:"loss"` // last hour, percent
	Line *string  `json:"line"` // classification label; nil until backhaul batch 2 lands
}

type serviceCarriers struct {
	CT serviceCarrier `json:"ct"`
	CU serviceCarrier `json:"cu"`
	CM serviceCarrier `json:"cm"`
}

// publicCarriers projects the node's carrierSummary into the public
// shape. It returns nil when no carrier has any figure to show (no
// targets, probing off, no samples yet), so callers omit the field
// entirely instead of publishing an empty shell.
func (s *Server) publicCarriers(n *store.Node, now time.Time) *serviceCarriers {
	sum, ok := s.carrierSummary(n, now)["carriers"].(gin.H)
	if !ok {
		return nil
	}
	proj := func(v carrierView) serviceCarrier {
		c := serviceCarrier{Loss: v.Loss1h, Line: v.Line}
		if v.Current != nil && v.Current.Ms > 0 {
			ms := v.Current.Ms
			c.Ms = &ms
		}
		return c
	}
	out := &serviceCarriers{}
	any := false
	for _, ck := range carrierKeys {
		v, ok := sum[ck].(carrierView)
		if !ok {
			continue
		}
		c := proj(v)
		if c.Ms != nil || c.Loss != nil || c.Line != nil {
			any = true
		}
		switch ck {
		case "ct":
			out.CT = c
		case "cu":
			out.CU = c
		case "cm":
			out.CM = c
		}
	}
	if !any {
		return nil
	}
	return out
}
