package cdnkit

import (
	"slices"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// RebuildLists rebuilds the preferred lists that origin's reports feed:
// Cloudflare profiles using the same HTTP/HTTPS scheme, or origin alone
// for another provider. A profile with fresh reports gets them merged,
// plus its typed-in domains; one with none keeps its list without the
// removed addresses (a retired point's). It returns the new lists and the
// profiles whose stale alert it cleared.
//
// A failed report query aborts the rebuild: it used to read as no
// reports, which could silently rebuild a list from nothing inside the
// caller's transaction.
func RebuildLists(st ReportStore, origin *store.CDNProfile, now time.Time, removed []string, removedPorts ...int) (map[string][]string, []store.CDNProfile, error) {
	targets, err := st.ProfilesSharingLists(origin)
	if err != nil {
		return nil, nil, err
	}
	lists := map[string][]string{}
	var recovered []store.CDNProfile
	for i := range targets {
		t := &targets[i]
		var upd map[string]any
		reports, err := SharedReports(st, t, now.Add(-StaleAfter(t)), "")
		if err != nil {
			return nil, nil, err
		}
		if len(reports) > 0 {
			list := MergedReportIPs(t, reports)
			upd = map[string]any{"preferred_ips": store.NewJSON(list), "last_refresh_at": now, "last_error": "", "stale_alerted": false}
			lists[t.ID] = list
			if t.StaleAlerted {
				recovered = append(recovered, *t)
			}
		} else if len(removed) > 0 && (len(removedPorts) == 0 || slices.Contains(removedPorts, DefaultPort(t))) {
			kept := slices.DeleteFunc(slices.Clone(t.PreferredIPs.V), func(ip string) bool {
				return slices.Contains(removed, ip) && !slices.Contains(t.PinnedHosts.V, ip)
			})
			upd = map[string]any{"preferred_ips": store.NewJSON(nonNilStrings(kept))}
			lists[t.ID] = kept
		} else {
			continue
		}
		if err := st.UpdateProfile(t, upd); err != nil {
			return nil, nil, err
		}
	}
	return lists, recovered, nil
}
