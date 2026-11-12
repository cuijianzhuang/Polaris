package panel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/panel/cdnkit"
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
	cdnNodeCheckNodes   = 3
	cdnNodeCheckMaxIPs  = 60
	cdnNodeCheckWorkers = 8
)

type cdnIPReportReq struct {
	Port    int                 `json:"port"`
	Scheme  string              `json:"scheme"`
	Source  string              `json:"source"`
	Region  string              `json:"region"`
	Carrier string              `json:"carrier"` // ct / cu / cm; "" = infer from source
	IPs     []store.CDNIPResult `json:"ips"`
}

// postCDNIPReport is POST /cdn-profiles/:id/preferred-ips {source, ips}.
func (s *Server) postCDNIPReport(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10)
	if t := apiTokenOf(c); t != nil && t.CDNUserID != "" {
		s.personalCDNReport(c, t)
		return
	}
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
	ips, err := cdnkit.CheckReport(req.Source, req.IPs)
	if err != nil {
		badRequest(c, err)
		return
	}
	carrier, err := cdnkit.ReportCarrier(req.Carrier, req.Source)
	if err != nil {
		badRequest(c, err)
		return
	}
	region, err := cdnkit.ReportRegion(req.Region, req.Source)
	if err != nil {
		badRequest(c, err)
		return
	}
	if token := apiTokenOf(c); token != nil && token.CDNRegion != "" {
		if req.Region != "" && region != token.CDNRegion {
			c.JSON(403, gin.H{"error": "测速令牌只能上报绑定的地区"})
			return
		}
		region = token.CDNRegion
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
			req.Scheme = cdnkit.Scheme(&p)
		}
	}
	now := time.Now().UTC()
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
		port, scheme, targetErr := cdnkit.ReportTarget(req.Port, req.Scheme, &p)
		if targetErr != nil {
			reportProblem = targetErr
			return targetErr
		}
		if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&store.CDNIPReport{
			ProfileID: p.ID, Source: req.Source, Carrier: carrier, Region: region, Port: port, Scheme: scheme, At: now, IPs: store.NewJSON(ips)}).Error; err != nil {
			return err
		}
		var err error
		lists, recovered, err = cdnkit.RebuildLists(dbStore{tx}, &p, now, nil)
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

// rebuildCDNLists lives in cdnkit now (lists.go); the SQL is dbStore in
// db_store.go. The callers below pass the transaction through it.

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
		if err := tx.Where("cdn_profile_id = ? AND cdn_source = ? AND (cdn_user_id = ? OR cdn_user_id IS NULL)", p.ID, c.Param("source"), "").Delete(&store.APIToken{}).Error; err != nil {
			return err
		}
		if len(gone) == 0 {
			return nil
		}
		ports := []int{}
		for _, r := range gone {
			ports = append(ports, cdnkit.EffectiveReportPort(r))
		}
		_, _, err := cdnkit.RebuildLists(dbStore{tx}, &p, time.Now().UTC(), cdnkit.ReportIPs(gone), ports...)
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
	rs, _ := cdnkit.SharedReports(dbStore{s.st.DB}, p, now.Add(-cdnkit.StaleAfter(p)), "")
	return len(rs) > 0
}

// checkCDNReportsStale alerts once per profile whose speed-test points all
// went quiet; the last list stays in use.
func (s *Server) checkCDNReportsStale() {
	now := time.Now().UTC()
	var ps []store.CDNProfile
	s.st.DB.Where("stale_alerted = ?", false).Find(&ps)
	for i := range ps {
		p := &ps[i]
		// The newest report this profile's list is built from: its own or,
		// for a Cloudflare profile, any Cloudflare profile's.
		rs, _ := cdnkit.SharedReports(dbStore{s.st.DB}, p, time.Time{}, "")
		if len(rs) == 0 {
			continue // no speed-test point: nothing to watch
		}
		last := rs[0]
		if now.Sub(last.At) < cdnkit.StaleAfter(p) {
			continue
		}
		if err := s.st.DB.Model(p).Update("stale_alerted", true).Error; err != nil {
			s.log.Warn("cdn stale flag", "profile", p.Name, "err", err)
			continue
		}
		fallback := "继续使用原有 IP 列表"
		if p.StalePolicy != "keep" && p.IPSourceURL != "" {
			if _, err := s.refreshPreferredIPs(s.ctxBG(), p, true); err != nil {
				s.recordCDNAutoSourceError(p, err)
				fallback = "来源 URL 刷新失败，保留原有列表"
			} else {
				fallback = "已尝试来源 URL 备用列表"
			}
		}
		s.notify(Event{Kind: "cdn_ips_stale", ResourceID: p.ID,
			Text: fmt.Sprintf("⚠️ CDN 配置档 %s 已 %s 没有收到优选 IP 上报（最后一次来自 %s），%s", p.Name, roundAgo(now.Sub(last.At)), last.Source, fallback),
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
			if check, ok := checks[strconv.Itoa(cdnkit.DefaultPort(p))]; ok {
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
	bad, _ := s.cdnUnreachablePort(ctx, p, nodes, cdnkit.DefaultPort(p))
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
		return nonNilStrs(bad), true
	}
	return nonNilStrs(bad), false
}

func nonNilReports(rs []store.CDNIPReport) []store.CDNIPReport {
	if rs == nil {
		return []store.CDNIPReport{}
	}
	return rs
}
