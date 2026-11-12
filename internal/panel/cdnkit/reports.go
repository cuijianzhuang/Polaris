package cdnkit

import (
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// SelectionCutoff is the earliest report time a profile's list is built
// from: the stale window back, or the beginning of time for a keep policy
// (a keep policy retains the last results for each port and carrier,
// rather than silently replacing an expired port with the default port's
// list).
func SelectionCutoff(p *store.CDNProfile) time.Time {
	if p.StalePolicy == "keep" {
		return time.Time{}
	}
	return time.Now().UTC().Add(-StaleAfter(p))
}

// ReportsForPort returns the reports a profile's list is built from for
// one port since a time: the newest per speed-test point, optionally of
// one carrier or region. A failed query is an error, not an empty list:
// callers that used to read a failure as "no reports" ignore it.
func ReportsForPort(st ReportStore, p *store.CDNProfile, port int, since time.Time, carrier string, regions ...string) ([]store.CDNIPReport, error) {
	rs, err := st.ReportsForPort(p, port, since, carrier)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := rs[:0]
	for _, r := range rs {
		if len(regions) > 0 && regions[0] != "" && EffectiveRegion(r) != regions[0] {
			continue
		}
		if !seen[r.Source] {
			seen[r.Source] = true
			out = append(out, r)
		}
	}
	return out, nil
}

// SharedReports is the reports a profile's list is built from since a
// time: Cloudflare profiles with the same HTTP/HTTPS scheme (its own
// otherwise), the newest per speed-test point, optionally of one carrier.
func SharedReports(st ReportStore, p *store.CDNProfile, since time.Time, carrier string) ([]store.CDNIPReport, error) {
	return ReportsForPort(st, p, DefaultPort(p), since, carrier)
}

// AudienceReports ranks the reports for one audience. Reports are
// isolated by the actual protocol and port before audience ranking.
// Known carrier: exact province+carrier, then that carrier nationwide,
// then merged. Unknown carrier: the province's reports, then merged. Old
// unlabeled reports still participate in the merged pool, or infer
// province from their source name. The second return value names the
// match: region_carrier, region, carrier or merged.
func AudienceReports(st ReportStore, p *store.CDNProfile, port int, carrier, region string) ([]store.CDNIPReport, string, error) {
	if p.RegionSplit && region != "" {
		filterCarrier := ""
		if p.CarrierSplit {
			filterCarrier = carrier
		}
		reports, err := ReportsForPort(st, p, port, SelectionCutoff(p), filterCarrier, region)
		if err != nil {
			return nil, "merged", err
		}
		if len(reports) > 0 {
			if filterCarrier != "" {
				return reports, "region_carrier", nil
			}
			return reports, "region", nil
		}
	}
	if p.CarrierSplit && carrier != "" {
		reports, err := ReportsForPort(st, p, port, SelectionCutoff(p), carrier)
		if err != nil {
			return nil, "merged", err
		}
		if len(reports) > 0 {
			return reports, "carrier", nil
		}
	}
	return nil, "merged", nil
}

// ReportsByProfiles returns the reports of the given profiles, newest
// first, grouped by profile with each report's region normalised.
func ReportsByProfiles(st ReportStore, ids []string) (map[string][]store.CDNIPReport, error) {
	out := map[string][]store.CDNIPReport{}
	if len(ids) == 0 {
		return out, nil
	}
	rs, err := st.ReportsByProfiles(ids)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		r.Region = EffectiveRegion(r)
		out[r.ProfileID] = append(out[r.ProfileID], r)
	}
	return out, nil
}

// nonNilStrings is store.NewJSON's counterpart for a list that must
// encode as [] rather than null.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
