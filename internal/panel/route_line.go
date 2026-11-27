package panel

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

// Heuristic return-path line classification (design
// docs/design/backhaul-monitor.md batch 2, D4–D8). The classifier is a
// pure panel-side function over a finished trace's hops and their frozen
// ASN annotations; it never re-probes and never sees Agent data. Labels
// are heuristics, not carrier facts: confidence and the "suspect"
// wording are part of the product contract and must not be dropped.

const routeClassifierVersion = 1

// routeLineRank orders lines within one carrier (design D8); only
// same-carrier comparisons are meaningful.
var routeLineRank = map[string]int{
	"163": 1, "cn2": 2,
	"4837": 1, "9929": 2,
	"cmnet": 1, "cmi": 2, "cmin2": 3,
}

var routeLineCarrier = map[string]string{
	"cn2": "ct", "163": "ct",
	"9929": "cu", "4837": "cu",
	"cmin2": "cm", "cmi": "cm", "cmnet": "cm",
}

// routeLinePremium marks each carrier's top tier; a streak of unknown
// results only alerts when it erases a premium line (design D8).
var routeLinePremium = map[string]bool{"cn2": true, "9929": true, "cmin2": true}

type routeSegmentFeature struct {
	prefix  netip.Prefix
	match   string // evidence display, e.g. "59.43.*"
	rule    string
	carrier string
	line    string
}

// Segment evidence outranks ASN evidence (design D5). The 9929 segments
// are the ones the design names; extend the table, not the logic, when
// new segments are confirmed.
var routeSegmentFeatures = []routeSegmentFeature{
	{netip.MustParsePrefix("59.43.0.0/16"), "59.43.*", "cn2-segment", "ct", "cn2"},
	{netip.MustParsePrefix("202.97.0.0/16"), "202.97.*", "163-segment", "ct", "163"},
	{netip.MustParsePrefix("210.51.0.0/16"), "210.51.*", "9929-segment", "cu", "9929"},
	{netip.MustParsePrefix("218.105.0.0/16"), "218.105.*", "9929-segment", "cu", "9929"},
}

type routeASNFeature struct {
	rule    string
	carrier string
	line    string
}

var routeASNFeatures = map[uint32]routeASNFeature{
	4134:  {"163-asn", "ct", "163"}, // AS4134 is the 163 backbone (design D5)
	9929:  {"9929-asn", "cu", "9929"},
	4837:  {"4837-asn", "cu", "4837"},
	58807: {"cmin2-asn", "cm", "cmin2"},
	58453: {"cmi-asn", "cm", "cmi"},
	9808:  {"cmnet-asn", "cm", "cmnet"},
}

// routeLineHit is one matched feature in the evidence summary. It records
// the feature (segment prefix or ASN), never the hop's full address.
type routeLineHit struct {
	TTL   int    `json:"ttl"`
	Match string `json:"match"`
	Rule  string `json:"rule"`
}

type routeLineCandidate struct {
	line       string
	confidence string // segment / asn / suspect
	ttl        int    // TTL of the determining hit closest to the target
	segment    bool   // determined by segment evidence
}

// classifyRoute is the pure classifier: (carrier, line, confidence,
// evidence) for one reached IPv4 path. No hits at all yields
// unknown/none — an honest unknown, never a guess.
func classifyRoute(hops []model.RouteTraceHop) (string, string, string, []routeLineHit) {
	ordered := append([]model.RouteTraceHop{}, hops...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].TTL < ordered[j].TTL })

	var hits []routeLineHit
	seen := map[routeLineHit]bool{}
	add := func(h routeLineHit) {
		if !seen[h] {
			seen[h] = true
			hits = append(hits, h)
		}
	}
	for _, h := range ordered {
		asnByIP := map[string]uint32{}
		for _, a := range h.ASN {
			if a.ASN != 0 {
				asnByIP[a.IP] = a.ASN
			}
		}
		for _, raw := range h.Addresses {
			addr, err := netip.ParseAddr(raw)
			if err != nil {
				continue
			}
			if addr.Is4() {
				for _, f := range routeSegmentFeatures {
					if f.prefix.Contains(addr) {
						add(routeLineHit{TTL: h.TTL, Match: f.match, Rule: f.rule})
					}
				}
			}
			if asn, ok := asnByIP[addr.String()]; ok {
				if f, ok := routeASNFeatures[asn]; ok {
					add(routeLineHit{TTL: h.TTL, Match: fmt.Sprintf("AS%d", asn), Rule: f.rule})
				}
			}
		}
	}
	maxTTL := func(rule string) (int, bool) {
		best, found := 0, false
		for _, h := range hits {
			if h.Rule == rule && (!found || h.TTL > best) {
				best, found = h.TTL, true
			}
		}
		return best, found
	}

	cands := map[string]*routeLineCandidate{}
	// Telecom: 59.43 anywhere means CN2; with no 202.97 segment anywhere
	// it is only *suspected* GIA (one-way evidence cannot separate
	// GIA from GT, design D5).
	if ttl, ok := maxTTL("cn2-segment"); ok {
		conf := "segment"
		if _, has163 := maxTTL("163-segment"); !has163 {
			conf = "suspect"
		}
		cands["ct"] = &routeLineCandidate{line: "cn2", confidence: conf, ttl: ttl, segment: true}
	} else if ttl, ok := maxTTL("163-segment"); ok {
		cands["ct"] = &routeLineCandidate{line: "163", confidence: "segment", ttl: ttl, segment: true}
	} else if ttl, ok := maxTTL("163-asn"); ok {
		cands["ct"] = &routeLineCandidate{line: "163", confidence: "asn", ttl: ttl}
	}
	// Unicom: 9929 evidence (segment first, then ASN) outranks 4837.
	if ttl, ok := maxTTL("9929-segment"); ok {
		cands["cu"] = &routeLineCandidate{line: "9929", confidence: "segment", ttl: ttl, segment: true}
	} else if ttl, ok := maxTTL("9929-asn"); ok {
		cands["cu"] = &routeLineCandidate{line: "9929", confidence: "asn", ttl: ttl}
	} else if ttl, ok := maxTTL("4837-asn"); ok {
		cands["cu"] = &routeLineCandidate{line: "4837", confidence: "asn", ttl: ttl}
	}
	// Mobile: ASN-only rules; the match closest to the target decides.
	{
		var best *routeLineCandidate
		for _, f := range routeASNFeatures {
			if f.carrier != "cm" {
				continue
			}
			ttl, ok := maxTTL(f.rule)
			if !ok {
				continue
			}
			c := &routeLineCandidate{line: f.line, confidence: "asn", ttl: ttl}
			if best == nil || c.ttl > best.ttl || (c.ttl == best.ttl && routeLineRank[c.line] > routeLineRank[best.line]) {
				best = c
			}
		}
		if best != nil {
			cands["cm"] = best
		}
	}

	// Cross-carrier conflicts resolve to the feature closest to the
	// target side (design D5); segment evidence breaks TTL ties.
	var winner *routeLineCandidate
	winnerCarrier := ""
	for _, ck := range carrierKeys {
		c := cands[ck]
		if c == nil {
			continue
		}
		if winner == nil || c.ttl > winner.ttl || (c.ttl == winner.ttl && c.segment && !winner.segment) {
			winner, winnerCarrier = c, ck
		}
	}
	if winner == nil {
		return "", "unknown", "none", hits
	}
	return winnerCarrier, winner.line, winner.confidence, hits
}

// classifyRouteTrace classifies one finished history row. Anything that
// did not reach its target (and every IPv6 trace, design D3) is
// unmeasured, not unknown: no path, no opinion.
func classifyRouteTrace(row store.NodeRouteTrace) (carrier, line, confidence, evidence string) {
	r := row.Result.V
	if row.Family != "ipv4" || row.State != "completed" || r.State != "reached" || len(r.Hops) == 0 {
		return "", "unmeasured", "none", ""
	}
	carrier, line, confidence, hits := classifyRoute(r.Hops)
	if len(hits) > 0 {
		if len(hits) > 16 {
			hits = hits[:16]
		}
		if b, err := json.Marshal(hits); err == nil {
			evidence = string(b)
		}
	}
	return carrier, line, confidence, evidence
}

func routeLineLabel(line, confidence string) string {
	switch line {
	case "cn2":
		if confidence == "suspect" {
			return "CN2（疑似 GIA）"
		}
		return "CN2"
	case "163":
		return "163"
	case "9929":
		return "9929"
	case "4837":
		return "4837"
	case "cmin2":
		return "CMIN2"
	case "cmi":
		return "CMI"
	case "cmnet":
		return "CMNET"
	case "unknown":
		return "无法识别"
	}
	return line
}

// routeLineClassifyTx stores the classification for a trace row that has
// just finished inside tx, and reports a route_line_changed event when
// the new result is a downgrade (or the third consecutive unknown after
// a premium line). Mirroring route_changed: only the newest completion
// in a scope may notify, and scheduled traces must still match the
// current schedule policy.
func routeLineClassifyTx(tx *gorm.DB, row store.NodeRouteTrace, now time.Time) (Event, bool, error) {
	carrier, line, confidence, evidence := classifyRouteTrace(row)
	rec := store.NodeRouteLine{
		TraceID: row.ID, NodeID: row.NodeID, Family: row.Family, Target: row.Target,
		Carrier: carrier, Line: line, Confidence: confidence, Evidence: evidence,
		ClassifierVersion: routeClassifierVersion, CreatedAt: now,
	}
	if err := tx.Create(&rec).Error; err != nil {
		return Event{}, false, err
	}
	if !routeScheduleNoticeCurrent(tx, row) {
		return Event{}, false, nil
	}
	var newer int64
	if err := tx.Model(&store.NodeRouteTrace{}).Where("node_id = ? AND family = ? AND target = ? AND id > ?", row.NodeID, row.Family, row.Target, row.ID).Count(&newer).Error; err != nil {
		return Event{}, false, err
	}
	if newer > 0 {
		return Event{}, false, nil
	}
	var prev []store.NodeRouteLine
	if err := tx.Where("node_id = ? AND family = ? AND target = ? AND trace_id < ?", row.NodeID, row.Family, row.Target, row.ID).Order("trace_id DESC").Limit(9).Find(&prev).Error; err != nil {
		return Event{}, false, err
	}

	var build func(node store.Node) Event
	if rank, classifiable := routeLineRank[line]; classifiable {
		// Compare against the previous *classifiable* result; unknown
		// and unmeasured rows in between do not break the chain, but a
		// different carrier does (not a downgrade, a different path).
		for i := range prev {
			pr, ok := routeLineRank[prev[i].Line]
			if !ok {
				continue
			}
			if prev[i].Carrier == carrier && pr > rank {
				from := prev[i]
				build = func(node store.Node) Event { return routeLineDownEvent(node, row, from, rec) }
			}
			break
		}
	} else if line == "unknown" {
		streak := 1
		var before *store.NodeRouteLine
		for i := range prev {
			if prev[i].Line == "unknown" {
				streak++
				continue
			}
			v := prev[i]
			before = &v
			break
		}
		if streak == 3 && before != nil && routeLinePremium[before.Line] {
			from := *before
			build = func(node store.Node) Event { return routeLineUnknownEvent(node, row, from, rec) }
		}
	}
	if build == nil {
		return Event{}, false, nil
	}
	var node store.Node
	if err := tx.Select("id", "name").First(&node, "id = ?", row.NodeID).Error; err != nil {
		return Event{}, false, err
	}
	return build(node), true, nil
}

// Do not fan hop addresses out to operators or webhooks (same rule as
// route_changed): events carry line labels only, the evidence summary
// stays in the administrator-only history detail.
func routeLineDownEvent(node store.Node, row store.NodeRouteTrace, from, cur store.NodeRouteLine) Event {
	return Event{
		Kind: "route_line_changed", NodeID: node.ID,
		ResourceID: notificationHash(row.Family, row.Target),
		Text:       fmt.Sprintf("📉 节点 %s 回程线路降级\n目标：%s（%s）\n线路：%s → %s\n历史记录：%d → %d\n可在回程检测历史中查看详情。", node.Name, row.Target, row.Family, routeLineLabel(from.Line, from.Confidence), routeLineLabel(cur.Line, cur.Confidence), from.TraceID, row.ID),
		Data:       map[string]any{"node_id": node.ID, "family": row.Family, "target": row.Target, "trace_id": row.ID, "previous_trace_id": from.TraceID, "from_line": from.Line, "to_line": cur.Line, "change": "downgrade"},
	}
}

func routeLineUnknownEvent(node store.Node, row store.NodeRouteTrace, from, cur store.NodeRouteLine) Event {
	return Event{
		Kind: "route_line_changed", NodeID: node.ID,
		ResourceID: notificationHash(row.Family, row.Target),
		Text:       fmt.Sprintf("❓ 节点 %s 回程线路连续 3 次无法识别\n目标：%s（%s）\n此前线路：%s\n历史记录：%d\n可在回程检测历史中查看详情。", node.Name, row.Target, row.Family, routeLineLabel(from.Line, from.Confidence), row.ID),
		Data:       map[string]any{"node_id": node.ID, "family": row.Family, "target": row.Target, "trace_id": row.ID, "previous_trace_id": from.TraceID, "from_line": from.Line, "to_line": cur.Line, "change": "unidentifiable"},
	}
}
