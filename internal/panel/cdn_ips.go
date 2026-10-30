package panel

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Self-maintained preferred IPs.
//
// Speed-test points (a small program on a machine where the users are, e.g.
// one per carrier) test CDN IPs from the users' side and report their best
// ones: POST /cdn-profiles/:id/preferred-ips (scope cdn:ips:write). The panel
// keeps each point's latest report and builds the profile's list from them.
// If no report arrives for StaleHours the admins are alerted and the last
// list stays in use.
//
// Independently, NodeCheck has online nodes connect to each preferred IP;
// IPs none of them can reach are left out of subscriptions until a later
// check finds them working again.

const (
	cdnReportMaxIPs     = 200
	cdnStaleDefault     = 24 // hours
	cdnNodeCheckNodes   = 3
	cdnNodeCheckMaxIPs  = 60
	cdnNodeCheckWorkers = 8
)

var (
	cdnSourceRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,39}$`)
	cdnColoRe   = regexp.MustCompile(`^[A-Z0-9]{2,5}$`)
)

type cdnIPReportReq struct {
	Port    int                 `json:"port"`
	Scheme  string              `json:"scheme"`
	Source  string              `json:"source"`
	Carrier string              `json:"carrier"` // ct / cu / cm; "" = infer from source
	IPs     []store.CDNIPResult `json:"ips"`
}

// checkCDNReport validates and ranks a report: lowest loss first, then
// fastest download (points that ran one), then lowest latency; duplicates
// keep their best result.
func checkCDNReport(req cdnIPReportReq) ([]store.CDNIPResult, error) {
	if !cdnSourceRe.MatchString(req.Source) {
		return nil, errors.New("source 为测速点名称，例如「上海电信」，最长 40 个字符")
	}
	if len(req.IPs) == 0 || len(req.IPs) > cdnReportMaxIPs {
		return nil, fmt.Errorf("每次上报 1–%d 个 IP", cdnReportMaxIPs)
	}
	seen := map[string]bool{}
	out := make([]store.CDNIPResult, 0, len(req.IPs))
	for _, r := range req.IPs {
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
		if colo != "" && !cdnColoRe.MatchString(colo) {
			return nil, fmt.Errorf("%s 的机房代码 %q 不合理（例如 HKG）", addr, r.Colo)
		}
		ip := addr.String()
		if seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, store.CDNIPResult{IP: ip, LatencyMs: r.LatencyMs, Loss: r.Loss, Speed: r.Speed, Colo: colo})
	}
	slices.SortStableFunc(out, func(a, b store.CDNIPResult) int {
		return cmp.Or(cmp.Compare(a.Loss, b.Loss), cmp.Compare(b.Speed, a.Speed), cmp.Compare(a.LatencyMs, b.LatencyMs))
	})
	return out, nil
}

// mergeCDNReports builds the preferred list from several points: their
// best IPs in turn (first of each, then second of each...), so every
// carrier's favourites come early. Sources are taken in name order.
func mergeCDNReports(reports []store.CDNIPReport) []string {
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

// cdnStaleAfter is how long a profile may go without a report.
func cdnStaleAfter(p *store.CDNProfile) time.Duration {
	h := p.StaleHours
	if h <= 0 {
		h = cdnStaleDefault
	}
	return time.Duration(h) * time.Hour
}

// postCDNIPReport is POST /cdn-profiles/:id/preferred-ips {source, ips}.
func (s *Server) postCDNIPReport(c *gin.Context) {
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req cdnIPReportReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	ips, err := checkCDNReport(req)
	if err != nil {
		badRequest(c, err)
		return
	}
	carrier, err := reportCarrier(req)
	if err != nil {
		badRequest(c, err)
		return
	}
	if err := cdnTokenAllows(apiTokenOf(c), p.ID, req.Source); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}
	if token := apiTokenOf(c); token != nil && token.CDNPort > 0 {
		if req.Port != 0 && req.Port != token.CDNPort {
			c.JSON(http.StatusForbidden, gin.H{"error": "测速令牌只能上报绑定的端口"})
			return
		}
		req.Port = token.CDNPort
		if req.Scheme == "" {
			req.Scheme = cdnScheme(&p)
		}
	}
	now := time.Now()
	var lists map[string][]string
	var recovered []store.CDNProfile
	var reportProblem error
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		if err := tx.First(&p, "id = ?", p.ID).Error; err != nil {
			return err
		}
		port, scheme, targetErr := cdnReportTarget(req, &p)
		if targetErr != nil {
			reportProblem = targetErr
			return targetErr
		}
		if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&store.CDNIPReport{
			ProfileID: p.ID, Source: req.Source, Carrier: carrier, Port: port, Scheme: scheme, At: now, IPs: store.NewJSON(ips)}).Error; err != nil {
			return err
		}
		var err error
		lists, recovered, err = rebuildCDNLists(tx, &p, now, nil)
		return err
	})
	if err != nil {
		if reportProblem != nil {
			badRequest(c, reportProblem)
		} else {
			dbError(c, err)
		}
		return
	}
	list := lists[p.ID]
	for _, r := range recovered {
		s.notify(Event{Kind: "cdn_ips_fresh", ResourceID: r.ID, Text: "✅ CDN 配置档 " + r.Name + " 的优选 IP 恢复上报（" + req.Source + "）",
			Data: map[string]any{"profile": r.ID, "source": req.Source}})
	}
	s.auditCtx(c, "cdn.ip_report", p.Name, fmt.Sprintf("%s: %d IPs", req.Source, len(ips)))
	c.JSON(http.StatusOK, gin.H{"stored": len(ips), "preferredIps": list})
}

// lockCDNReports serializes reports, manual domain edits and source refresh
// commits on PostgreSQL. Each writer must read the latest lists and pins after
// taking the lock so its update cannot undo another writer. SQLite already
// serializes writers.
func lockCDNReports(tx *gorm.DB) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "polaris-cdn-reports").Error
}

// rebuildCDNLists rebuilds the preferred lists that origin's reports feed:
// Cloudflare profiles using the same HTTP/HTTPS scheme, or origin alone
// for another provider. A profile with
// fresh reports gets them merged, plus its typed-in domains; one with none
// keeps its list without the removed addresses (a retired point's). It
// returns the new lists and the profiles whose stale alert it cleared.
func rebuildCDNLists(tx *gorm.DB, origin *store.CDNProfile, now time.Time, removed []string, removedPorts ...int) (map[string][]string, []store.CDNProfile, error) {
	var targets []store.CDNProfile
	q := tx.Where("id = ?", origin.ID)
	if origin.Provider == "cloudflare" {
		q = tx.Where("provider = ?", "cloudflare").Where("COALESCE(NULLIF(scheme, ''), 'https') = ?", cdnScheme(origin))
	}
	if err := q.Find(&targets).Error; err != nil {
		return nil, nil, err
	}
	lists := map[string][]string{}
	var recovered []store.CDNProfile
	for i := range targets {
		t := &targets[i]
		var upd map[string]any
		if reports := sharedCDNReports(tx, t, now.Add(-cdnStaleAfter(t)), ""); len(reports) > 0 {
			list := withPinned(mergeCDNReports(reports), t.PinnedHosts.V)
			upd = map[string]any{"preferred_ips": store.NewJSON(list), "last_refresh_at": now, "last_error": "", "stale_alerted": false}
			lists[t.ID] = list
			if t.StaleAlerted {
				recovered = append(recovered, *t)
			}
		} else if len(removed) > 0 && (len(removedPorts) == 0 || slices.Contains(removedPorts, cdnDefaultPort(t))) {
			kept := slices.DeleteFunc(slices.Clone(t.PreferredIPs.V), func(ip string) bool {
				return slices.Contains(removed, ip) && !slices.Contains(t.PinnedHosts.V, ip)
			})
			upd = map[string]any{"preferred_ips": store.NewJSON(nonNilStrs(kept))}
			lists[t.ID] = kept
		} else {
			continue
		}
		if err := tx.Model(t).Updates(upd).Error; err != nil {
			return nil, nil, err
		}
	}
	return lists, recovered, nil
}

// reportIPs lists the addresses of reports.
func reportIPs(rs []store.CDNIPReport) []string {
	var out []string
	for _, r := range rs {
		for _, x := range r.IPs.V {
			out = append(out, x.IP)
		}
	}
	return out
}

// sharedCDNReports is the reports a profile's list is built from since a
// time: Cloudflare profiles with the same HTTP/HTTPS scheme (its own
// otherwise), the newest per speed-test point, optionally of one carrier.
func sharedCDNReports(db *gorm.DB, p *store.CDNProfile, since time.Time, carrier string) []store.CDNIPReport {
	return cdnReportsForPort(db, p, cdnDefaultPort(p), since, carrier)
}

func cdnReportsForPort(db *gorm.DB, p *store.CDNProfile, port int, since time.Time, carrier string) []store.CDNIPReport {
	q := db.Model(&store.CDNIPReport{}).Where("at > ?", since)
	q = q.Where("COALESCE(NULLIF(port,0),443) = ?", port).Where("COALESCE(NULLIF(scheme,''),'https') = ?", cdnScheme(p))
	if p.Provider == "cloudflare" {
		q = q.Where("profile_id IN (?)", db.Model(&store.CDNProfile{}).Select("id").Where("provider = ?", "cloudflare").Where("COALESCE(NULLIF(scheme, ''), 'https') = ?", cdnScheme(p)))
	} else {
		q = q.Where("profile_id = ?", p.ID)
	}
	if carrier != "" {
		q = q.Where("carrier = ?", carrier)
	}
	var rs []store.CDNIPReport
	q.Order("at DESC").Find(&rs)
	seen := map[string]bool{}
	out := rs[:0]
	for _, r := range rs {
		if !seen[r.Source] {
			seen[r.Source] = true
			out = append(out, r)
		}
	}
	return out
}

// cdnReports returns the reports of the given profiles, newest first.
func (s *Server) cdnReports(ids []string) map[string][]store.CDNIPReport {
	out := map[string][]store.CDNIPReport{}
	if len(ids) == 0 {
		return out
	}
	var rs []store.CDNIPReport
	s.st.DB.Where("profile_id IN ?", ids).Order("at DESC").Find(&rs)
	for _, r := range rs {
		out[r.ProfileID] = append(out[r.ProfileID], r)
	}
	return out
}

// deleteCDNIPReport is DELETE /cdn-profiles/:id/ip-reports/:source: forget
// a speed-test point that was retired, and revoke its paired token.
func (s *Server) deleteCDNIPReport(c *gin.Context) {
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	// A paired point's token goes with it, and so do the addresses it
	// reported: the lists it fed are rebuilt from the points left.
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		if err := tx.First(&p, "id = ?", p.ID).Error; err != nil {
			return err
		}
		var gone []store.CDNIPReport
		if err := tx.Where("profile_id = ? AND source = ?", p.ID, c.Param("source")).Find(&gone).Error; err != nil {
			return err
		}
		if err := tx.Where("profile_id = ? AND source = ?", p.ID, c.Param("source")).Delete(&store.CDNIPReport{}).Error; err != nil {
			return err
		}
		if err := tx.Where("cdn_profile_id = ? AND cdn_source = ?", p.ID, c.Param("source")).Delete(&store.APIToken{}).Error; err != nil {
			return err
		}
		if len(gone) == 0 {
			return nil
		}
		ports := []int{}
		for _, r := range gone {
			ports = append(ports, effectiveCDNReportPort(r))
		}
		_, _, err := rebuildCDNLists(tx, &p, time.Now(), reportIPs(gone), ports...)
		return err
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "cdn.ip_report_delete", c.Param("id"), c.Param("source"))
	c.Status(http.StatusNoContent)
}

// cdnFreshReport reports whether a speed-test point reported within the
// stale window: the scheduled URL refresh then leaves the list alone.
func (s *Server) cdnFreshReport(p *store.CDNProfile, now time.Time) bool {
	return len(sharedCDNReports(s.st.DB, p, now.Add(-cdnStaleAfter(p)), "")) > 0
}

// checkCDNReportsStale alerts once per profile whose speed-test points all
// went quiet; the last list stays in use.
func (s *Server) checkCDNReportsStale() {
	now := time.Now()
	var ps []store.CDNProfile
	s.st.DB.Where("stale_alerted = ?", false).Find(&ps)
	for i := range ps {
		p := &ps[i]
		// The newest report this profile's list is built from: its own or,
		// for a Cloudflare profile, any Cloudflare profile's.
		rs := sharedCDNReports(s.st.DB, p, time.Time{}, "")
		if len(rs) == 0 {
			continue // no speed-test point: nothing to watch
		}
		last := rs[0]
		if now.Sub(last.At) < cdnStaleAfter(p) {
			continue
		}
		if err := s.st.DB.Model(p).Update("stale_alerted", true).Error; err != nil {
			s.log.Warn("cdn stale flag", "profile", p.Name, "err", err)
			continue
		}
		s.notify(Event{Kind: "cdn_ips_stale", ResourceID: p.ID,
			Text: fmt.Sprintf("⚠️ CDN 配置档 %s 已 %s 没有收到优选 IP 上报（最后一次来自 %s），继续使用原有 IP 列表", p.Name, roundAgo(now.Sub(last.At)), last.Source),
			Data: map[string]any{"profile": p.ID, "lastSource": last.Source, "lastAt": last.At}})
	}
}

func roundAgo(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
	return fmt.Sprintf("%d 小时", int(d.Hours()))
}

// checkCDNIPsFromNodes has up to three online nodes connect to each
// preferred IP of profiles with NodeCheck on. An IP every node fails to
// reach is marked bad; if every IP fails, nothing is marked (the nodes,
// not the IPs, are the likely problem).
func (s *Server) checkCDNIPsFromNodes() {
	var ps []store.CDNProfile
	s.st.DB.Where("node_check = ?", true).Find(&ps)
	if len(ps) == 0 {
		return
	}
	var nodes []string
	s.st.DB.Model(&store.Node{}).Where("status = ?", "online").Order("id").Limit(cdnNodeCheckNodes).Pluck("id", &nodes)
	if len(nodes) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctxBG(), 4*time.Minute)
	defer cancel()
	for i := range ps {
		p := &ps[i]
		checks := p.PortChecks.V
		if checks == nil {
			checks = map[string]store.CDNPortCheck{}
		}
		ports := s.cdnCheckPorts(p)
		slices.SortStableFunc(ports, func(a, b int) int { return checks[strconv.Itoa(a)].At.Compare(checks[strconv.Itoa(b)].At) })
		changed := false
		for _, port := range ports {
			q := *p
			q.PreferredIPs = store.NewJSON(s.cdnAddressesForPort(p, port))
			if len(q.PreferredIPs.V) == 0 {
				continue
			}
			bad, allFailed := s.cdnUnreachablePort(ctx, &q, nodes, port)
			if ctx.Err() != nil {
				break
			}
			checks[strconv.Itoa(port)] = store.CDNPortCheck{BadIPs: bad, AllFailed: allFailed, At: time.Now()}
			changed = true
		}
		if changed {
			upd := map[string]any{"port_checks": store.NewJSON(checks)}
			if check, ok := checks[strconv.Itoa(cdnDefaultPort(p))]; ok {
				upd["bad_ips"] = store.NewJSON(check.BadIPs)
				upd["checked_at"] = check.At
			}
			if err := s.st.DB.Model(p).Updates(upd).Error; err != nil {
				s.log.Warn("cdn node check", "profile", p.Name, "err", err)
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// cdnUnreachable returns the IPs (of the first cdnNodeCheckMaxIPs) that
// failed from every node.
func (s *Server) cdnUnreachable(ctx context.Context, p *store.CDNProfile, nodes []string) []string {
	bad, _ := s.cdnUnreachablePort(ctx, p, nodes, cdnDefaultPort(p))
	return bad
}
func (s *Server) cdnUnreachablePort(ctx context.Context, p *store.CDNProfile, nodes []string, port int) ([]string, bool) {
	ips := p.PreferredIPs.V
	if len(ips) > cdnNodeCheckMaxIPs {
		ips = ips[:cdnNodeCheckMaxIPs]
	}
	var mu sync.Mutex
	ok := map[string]bool{}
	sem := make(chan struct{}, cdnNodeCheckWorkers)
	var wg sync.WaitGroup
	for _, ip := range ips {
		for _, n := range nodes {
			wg.Add(1)
			go func(ip, n string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if _, reached := s.probeOnce(ctx, n, net.JoinHostPort(ip, strconv.Itoa(port))); reached {
					mu.Lock()
					ok[ip] = true
					mu.Unlock()
				}
			}(ip, n)
		}
	}
	wg.Wait()
	var bad []string
	for _, ip := range ips {
		if !ok[ip] {
			bad = append(bad, ip)
		}
	}
	if len(ips) > 0 && len(bad) == len(ips) {
		return []string{}, true
	}
	return nonNilStrs(bad), false
}

func nonNilReports(rs []store.CDNIPReport) []store.CDNIPReport {
	if rs == nil {
		return []store.CDNIPReport{}
	}
	return rs
}

// usableCDNIPs is the profile's preferred list without the IPs the node
// check marked bad.
func usableCDNIPs(p *store.CDNProfile) []string {
	return usableCDNIPsAt(p, p.PreferredIPs.V, cdnDefaultPort(p))
}
