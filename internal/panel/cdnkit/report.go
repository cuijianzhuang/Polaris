package cdnkit

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Self-maintained preferred IPs.
//
// Speed-test points (a small program on a machine where the users are, e.g.
// one per carrier) test CDN IPs from the users' side and report their best
// ones. The panel keeps each point's latest report and builds the profile's
// list from them. If no report arrives for StaleHours the admins are alerted
// and the last list stays in use.
//
// Independently, NodeCheck has online nodes connect to each preferred IP;
// IPs none of them can reach are left out of subscriptions until a later
// check finds them working again.

// ReportMaxIPs is the most IPs one report may carry.
const ReportMaxIPs = 200

// staleDefault is the stale window in hours when a profile sets none.
const staleDefault = 24

var (
	sourceRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,39}$`)
	coloRe   = regexp.MustCompile(`^[A-Z0-9]{2,5}$`)
)

// ValidSourceName reports whether a speed-test point name is acceptable:
// a letter, digit or CJK character first, at most 40 characters.
func ValidSourceName(source string) bool {
	return sourceRe.MatchString(source)
}

// CheckReport validates and ranks a report: lowest loss first, then
// fastest download (points that ran one), then lowest latency; duplicates
// keep their best result.
func CheckReport(source string, ips []store.CDNIPResult) ([]store.CDNIPResult, error) {
	if !sourceRe.MatchString(source) {
		return nil, errors.New("source 为测速点名称，例如「上海电信」，最长 40 个字符")
	}
	if len(ips) == 0 || len(ips) > ReportMaxIPs {
		return nil, fmt.Errorf("每次上报 1–%d 个 IP", ReportMaxIPs)
	}
	seen := map[string]int{}
	out := make([]store.CDNIPResult, 0, len(ips))
	for _, r := range ips {
		addr, err := netip.ParseAddr(strings.TrimSpace(r.IP))
		if err != nil {
			return nil, fmt.Errorf("%q 不是 IP 地址", r.IP)
		}
		addr = addr.Unmap()
		if !addr.IsGlobalUnicast() || addr.IsPrivate() {
			return nil, fmt.Errorf("%s 不是公网 IP", addr)
		}
		if r.LatencyMs < 0 || r.LatencyMs > 60000 || r.Loss < 0 || r.Loss > 1 {
			return nil, fmt.Errorf("%s 的延迟或丢包率不合理（latencyMs 0–60000，loss 0–1）", addr)
		}
		if r.Speed < 0 || r.Speed > 100000 {
			return nil, fmt.Errorf("%s 的下载速度不合理（speed 为 MB/s，0–100000）", addr)
		}
		colo := strings.ToUpper(strings.TrimSpace(r.Colo))
		if colo != "" && !coloRe.MatchString(colo) {
			return nil, fmt.Errorf("%s 的机房代码 %q 不合理（例如 HKG）", addr, r.Colo)
		}
		ip := addr.String()
		result := store.CDNIPResult{IP: ip, LatencyMs: r.LatencyMs, Loss: r.Loss, Speed: r.Speed, Colo: colo}
		if i, exists := seen[ip]; exists {
			if CompareIPResults(result, out[i]) < 0 {
				out[i] = result
			}
			continue
		}
		seen[ip] = len(out)
		out = append(out, result)
	}

	slices.SortStableFunc(out, CompareIPResults)
	return out, nil
}

// CompareIPResults orders two results the way deduplication and sorting
// must agree on: lowest loss, then fastest download, then lowest latency.
func CompareIPResults(a, b store.CDNIPResult) int {
	return cmp.Or(cmp.Compare(a.Loss, b.Loss), cmp.Compare(b.Speed, a.Speed), cmp.Compare(a.LatencyMs, b.LatencyMs))
}

// MergedReportIPs merges speed-test reports into the preferred list, with
// pinned hosts kept in place. Shared by subscription rendering, the
// operator preview and the scheduled list rebuild.
func MergedReportIPs(p *store.CDNProfile, reports []store.CDNIPReport) []string {
	return WithPinned(MergeReports(reports), p.PinnedHosts.V)
}

// UsableReportIPs is MergedReportIPs with the node-check filter applied.
func UsableReportIPs(p *store.CDNProfile, reports []store.CDNIPReport, port int) []string {
	return UsableIPsAt(p, MergedReportIPs(p, reports), port)
}

// MergeReports builds the preferred list from several points: their best
// IPs in turn (first of each, then second of each...), so every carrier's
// favourites come early. Sources are taken in name order.
func MergeReports(reports []store.CDNIPReport) []string {
	slices.SortFunc(reports, func(a, b store.CDNIPReport) int { return strings.Compare(a.Source, b.Source) })
	seen := map[string]bool{}
	var out []string
	for i := 0; ; i++ {
		more := false
		for _, r := range reports {
			if i >= len(r.IPs.V) {
				continue
			}
			more = true
			if ip := r.IPs.V[i].IP; !seen[ip] {
				seen[ip] = true
				out = append(out, ip)
			}
		}
		if !more {
			return out
		}
	}
}

// StaleAfter is how long a profile may go without a report.
func StaleAfter(p *store.CDNProfile) time.Duration {
	h := p.StaleHours
	if h <= 0 {
		h = staleDefault
	}
	return time.Duration(h) * time.Hour
}

// ReportIPs lists the addresses of reports.
func ReportIPs(rs []store.CDNIPReport) []string {
	var out []string
	for _, r := range rs {
		for _, x := range r.IPs.V {
			out = append(out, x.IP)
		}
	}
	return out
}

// ReportTarget resolves the port and scheme a report is filed under. A
// report that names neither inherits the profile's scheme and files under
// port 0 (the default port's bucket).
func ReportTarget(port int, scheme string, p *store.CDNProfile) (int, string, error) {
	if port == 0 && scheme == "" && Scheme(p) == "https" {
		return 0, "", nil
	}
	if scheme != "" && scheme != Scheme(p) {
		return 0, "", fmt.Errorf("测速协议与 CDN 配置档不一致")
	}
	if port < 1 || port > 65535 || (p.Provider == "cloudflare" && !slices.Contains(ClientPorts(p), port)) {
		return 0, "", fmt.Errorf("请上报当前 CDN 协议支持的实际测速端口")
	}
	return port, Scheme(p), nil
}

// EffectiveReportPort is the port a report counts for (0 = the default).
func EffectiveReportPort(r store.CDNIPReport) int {
	if r.Port == 0 {
		return 443
	}
	return r.Port
}

// EffectiveReportScheme is the scheme a report counts for ("" = HTTPS).
func EffectiveReportScheme(r store.CDNIPReport) string {
	if r.Scheme == "" {
		return "https"
	}
	return r.Scheme
}

// AddressBlocked reports whether an address is on the profile's block list
// (IP equality or a case-insensitive host match).
func AddressBlocked(p *store.CDNProfile, address string) bool {
	for _, a := range p.BlockedHosts.V {
		ai, ae := netip.ParseAddr(a)
		bi, be := netip.ParseAddr(address)
		if ae == nil && be == nil && ai.Unmap() == bi.Unmap() {
			return true
		}
		if strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(address, ".")) {
			return true
		}
	}
	return false
}

// UsableIPsAt is a preferred list without the IPs the node check marked bad
// for this port (and without blocked addresses).
func UsableIPsAt(p *store.CDNProfile, list []string, port int) []string {
	list = slices.DeleteFunc(slices.Clone(list), func(a string) bool { return AddressBlocked(p, a) })
	if !p.NodeCheck {
		return list
	}
	var bad []string
	if check, ok := p.PortChecks.V[strconv.Itoa(port)]; ok {
		bad = check.BadIPs
	} else if port == DefaultPort(p) {
		bad = p.BadIPs.V
	}
	out := slices.DeleteFunc(slices.Clone(list), func(ip string) bool { return slices.Contains(bad, ip) })
	return out
}

// UsableIPs is the profile's preferred list without the IPs the node check
// marked bad.
func UsableIPs(p *store.CDNProfile) []string {
	return UsableIPsAt(p, p.PreferredIPs.V, DefaultPort(p))
}

// WithPinned is an automatic list of preferred addresses plus the domains
// the admin typed in, which automatic updates never drop.
func WithPinned(list, pinned []string) []string {
	out := slices.Clone(list)
	for _, h := range pinned {
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}
