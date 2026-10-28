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
	now := time.Now()
	var list []string
	var recovered bool
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&store.CDNIPReport{
			ProfileID: p.ID, Source: req.Source, Carrier: carrier, At: now, IPs: store.NewJSON(ips)}).Error; err != nil {
			return err
		}
		var reports []store.CDNIPReport
		if err := tx.Where("profile_id = ? AND at > ?", p.ID, now.Add(-cdnStaleAfter(&p))).Find(&reports).Error; err != nil {
			return err
		}
		list = mergeCDNReports(reports)
		recovered = p.StaleAlerted
		return tx.Model(&p).Updates(map[string]any{"preferred_ips": store.NewJSON(list), "last_refresh_at": now,
			"last_error": "", "stale_alerted": false}).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	if recovered {
		s.notify(Event{Kind: "cdn_ips_fresh", ResourceID: p.ID, Text: "✅ CDN 配置档 " + p.Name + " 的优选 IP 恢复上报（" + req.Source + "）",
			Data: map[string]any{"profile": p.ID, "source": req.Source}})
	}
	s.auditCtx(c, "cdn.ip_report", p.Name, fmt.Sprintf("%s: %d IPs", req.Source, len(ips)))
	c.JSON(http.StatusOK, gin.H{"stored": len(ips), "preferredIps": list})
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
// a speed-test point that was retired.
func (s *Server) deleteCDNIPReport(c *gin.Context) {
	if err := s.st.DB.Where("profile_id = ? AND source = ?", c.Param("id"), c.Param("source")).Delete(&store.CDNIPReport{}).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "cdn.ip_report_delete", c.Param("id"), c.Param("source"))
	c.Status(http.StatusNoContent)
}

// cdnFreshReport reports whether a speed-test point reported within the
// stale window: the scheduled URL refresh then leaves the list alone.
func (s *Server) cdnFreshReport(p *store.CDNProfile, now time.Time) bool {
	var n int64
	s.st.DB.Model(&store.CDNIPReport{}).Where("profile_id = ? AND at > ?", p.ID, now.Add(-cdnStaleAfter(p))).Count(&n)
	return n > 0
}

// checkCDNReportsStale alerts once per profile whose speed-test points all
// went quiet; the last list stays in use.
func (s *Server) checkCDNReportsStale() {
	now := time.Now()
	var ps []store.CDNProfile
	s.st.DB.Where("stale_alerted = ?", false).Find(&ps)
	for i := range ps {
		p := &ps[i]
		var last store.CDNIPReport
		if s.st.DB.Where("profile_id = ?", p.ID).Order("at DESC").First(&last).Error != nil {
			continue // no speed-test point: nothing to watch
		}
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
		bad := s.cdnUnreachable(ctx, &ps[i], nodes)
		if ctx.Err() != nil {
			return // ran out of time: keep the previous verdict
		}
		if err := s.st.DB.Model(&ps[i]).Updates(map[string]any{"bad_ips": store.NewJSON(bad), "checked_at": time.Now()}).Error; err != nil {
			s.log.Warn("cdn node check", "profile", ps[i].Name, "err", err)
		}
	}
}

// cdnUnreachable returns the IPs (of the first cdnNodeCheckMaxIPs) that
// failed from every node.
func (s *Server) cdnUnreachable(ctx context.Context, p *store.CDNProfile, nodes []string) []string {
	ips := p.PreferredIPs.V
	if len(ips) > cdnNodeCheckMaxIPs {
		ips = ips[:cdnNodeCheckMaxIPs]
	}
	port := 443
	if len(p.Ports.V) > 0 {
		port = p.Ports.V[0]
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
	if len(bad) == len(ips) {
		return []string{}
	}
	return nonNilStrs(bad)
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
	if !p.NodeCheck || len(p.BadIPs.V) == 0 {
		return p.PreferredIPs.V
	}
	out := make([]string, 0, len(p.PreferredIPs.V))
	for _, ip := range p.PreferredIPs.V {
		if !slices.Contains(p.BadIPs.V, ip) {
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		return p.PreferredIPs.V
	}
	return out
}
