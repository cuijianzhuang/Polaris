package panel

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var errPersonalCDNConflict = errors.New("个人地址池已在其他窗口更新，当前草稿已保留，请刷新并对照最新内容后再保存")

func personalCDNVersion(user, profile string, manual []string, pool store.PersonalCDNPool) string {
	// Measurements, checks and credentials do not invalidate a manual edit.
	b, _ := json.Marshal([]any{user, profile, nonNilStrs(manual), nonNilStrs(pool.Disabled), personalCDNPolicy(pool), personalCDNHours(pool)})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func personalCDNPolicy(pool store.PersonalCDNPool) string {
	if pool.StalePolicy == "fallback" {
		return "fallback"
	}
	return "keep"
}
func personalCDNHours(pool store.PersonalCDNPool) int {
	if pool.StaleHours > 0 {
		return pool.StaleHours
	}
	return 24
}
func personalCDNReportAt(pool store.PersonalCDNPool, port int) time.Time {
	if meta, ok := pool.PortReports[strconv.Itoa(port)]; ok {
		return meta.At
	}
	if pool.ReportedAt != nil {
		return *pool.ReportedAt
	}
	return time.Time{}
}
func personalCDNExpired(pool store.PersonalCDNPool, port int, now time.Time) bool {
	at := personalCDNReportAt(pool, port)
	return at.IsZero() || !now.Before(at.Add(time.Duration(personalCDNHours(pool))*time.Hour))
}
func personalCDNAutomatic(pool store.PersonalCDNPool, port int, scheme string, now time.Time) []string {
	if meta, ok := pool.PortReports[strconv.Itoa(port)]; ok && scheme != "" && meta.Scheme != "" && meta.Scheme != scheme {
		return nil
	}
	if personalCDNPolicy(pool) == "fallback" && personalCDNExpired(pool, port, now) {
		return nil
	}
	return pool.Automatic[strconv.Itoa(port)]
}
func personalCDNFailure(pool store.PersonalCDNPool, ep string, target cdnTarget, tr model.Transport, revision string) *store.PersonalCDNCheck {
	r, ok := pool.EndpointChecks[cdnFailureSlot(ep, target.Server)]
	if !ok || r.Fingerprint != cdnCheckKey(ep, target, tr, revision) || !r.Failed {
		return nil
	}
	return &r
}
func recordPersonalCDNVerdict(pool *store.PersonalCDNPool, ep, address string, result store.PersonalCDNCheck) store.PersonalCDNCheck {
	if pool.EndpointChecks == nil {
		pool.EndpointChecks = map[string]store.PersonalCDNCheck{}
	}
	slot := cdnFailureSlot(ep, address)
	previous := pool.EndpointChecks[slot]
	if previous.At.After(result.At) {
		return previous
	}
	result.Failed = !result.OK && (!result.Partial || (previous.Fingerprint == result.Fingerprint && previous.Failed))
	pool.EndpointChecks[slot] = result
	return result
}

func personalCDNDialer() *net.Dialer {
	return &net.Dialer{Timeout: 8 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		return checkSourceIP(net.ParseIP(host), false)
	}}
}

type personalCDNEntry struct {
	cdnCheckContext
	Context string                            `json:"context"`
	Checks  map[string]store.PersonalCDNCheck `json:"checks"`
}

// Restrict entry metadata before resolving it: another entry using the same
// shared profile does not become visible or testable through that profile.
func (s *Server) personalCDNEntries(db *gorm.DB, u *store.User, p *store.CDNProfile) ([]personalCDNEntry, error) {
	var es []store.Endpoint
	err := db.Table("endpoints").Select("DISTINCT endpoints.*").Joins("JOIN inbounds ON inbounds.id = endpoints.inbound_id").Joins("JOIN nodes ON nodes.id = inbounds.node_id").Joins("JOIN ("+store.GrantsSQL+") grants ON grants.inbound_id = inbounds.id").Where("grants.user_id = ? AND endpoints.cdn_profile_id = ? AND endpoints.kind = ? AND inbounds.enabled = ? AND inbounds.service_only = ? AND inbounds.relay_only = ? AND nodes.revoked = ?", u.ID, p.ID, "cdn", true, false, false, false).Order("endpoints.id").Find(&es).Error
	if err != nil {
		return nil, err
	}
	entries := []personalCDNEntry{}
	for _, ep := range es {
		var row store.Inbound
		if err := db.First(&row, "id = ?", ep.InboundID).Error; err != nil {
			return nil, err
		}
		m := s.toModelInbound(&row)
		if master := s.fallbackMasters([]string{row.NodeID})[row.ID]; master != nil {
			mm := s.toModelInbound(master)
			m.Port, m.Security = mm.Port, mm.Security
		}
		if !m.CDNCapable() {
			continue
		}
		var node store.Node
		if err := db.First(&node, "id = ?", row.NodeID).Error; err != nil {
			return nil, err
		}
		entry := personalCDNEntry{cdnCheckContext: cdnCheckContext{EndpointID: ep.ID, InboundID: row.ID, Label: node.Name + " / " + row.Tag, ExplicitAddress: ep.Address, Transport: m.Transport.Type, Revision: cdnHealthRevision(row, p), Target: s.cdnTargetFor(&ep, &m, p)}, Checks: map[string]store.PersonalCDNCheck{}}
		signatureTarget := entry.Target
		signatureTarget.Server = ""
		entry.Context = cdnCheckKey(ep.ID, signatureTarget, m.Transport.Type, entry.Revision)
		pool := u.CDNPool.V[p.ID]
		candidates := append(slices.Clone(u.CDNPreferred.V[p.ID]), pool.Automatic[strconv.Itoa(entry.Target.Port)]...)
		candidates = append(candidates, entry.Target.Server)
		for _, address := range candidates {
			target := entry.Target
			target.Server = address
			if r, ok := pool.EndpointChecks[cdnFailureSlot(ep.ID, address)]; ok && r.Fingerprint == cdnCheckKey(ep.ID, target, m.Transport.Type, entry.Revision) {
				entry.Checks[address] = r
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
func personalCDNProbeHost(entries []personalCDNEntry, ep string, port int, fallback string) string {
	for _, entry := range entries {
		if entry.EndpointID == ep && entry.Target.Port == port {
			return entry.Target.SNI
		}
	}
	return fallback
}
func (s *Server) personalCDNState(c *gin.Context) {
	u := s.portalUser(c)
	if u == nil {
		c.JSON(401, gin.H{"error": "请重新登录"})
		return
	}
	profiles, err := s.cdnUserProfiles(u)
	if err != nil {
		dbError(c, err)
		return
	}
	index := slices.IndexFunc(profiles, func(p store.CDNProfile) bool { return p.ID == c.Query("profileId") })
	if index < 0 {
		c.JSON(403, gin.H{"error": "只能查看本人有权限使用的 CDN 配置"})
		return
	}
	p := profiles[index]
	entries, err := s.personalCDNEntries(s.st.DB, u, &p)
	if err != nil {
		dbError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"version": personalCDNVersion(u.ID, p.ID, u.CDNPreferred.V[p.ID], u.CDNPool.V[p.ID]), "addresses": nonNilStrs(u.CDNPreferred.V[p.ID]), "pool": u.CDNPool.V[p.ID], "entries": entries})
}

type personalCDNPreviewRow struct {
	Address  string `json:"address"`
	Source   string `json:"source"`
	Selected bool   `json:"selected"`
	Reason   string `json:"reason"`
}
type personalCDNPreviewEntry struct {
	EndpointID string                  `json:"endpointId"`
	Label      string                  `json:"label"`
	Target     cdnTarget               `json:"target"`
	Mode       string                  `json:"mode"`
	Selected   []string                `json:"selected"`
	Rows       []personalCDNPreviewRow `json:"rows"`
}

func (s *Server) personalCDNPreviewResponse(c *gin.Context, u *store.User, p *store.CDNProfile, entries []personalCDNEntry) {
	trace := map[string][]sub.Node{}
	carrier, region := s.subCarrier(c.Request, u), s.subRegion(c.Request, u)
	available := s.servedUser(u)
	if available {
		if _, err := s.subNodesForTrace(u, carrier, region, trace); err != nil {
			dbError(c, err)
			return
		}
	}
	result := []personalCDNPreviewEntry{}
	for _, entry := range entries {
		pool := u.CDNPool.V[p.ID]
		port := entry.Target.Port
		selected := []string{}
		for _, node := range trace[entry.EndpointID] {
			selected = append(selected, node.Server)
		}
		mode := "public"
		if !p.ExpandPerIP || entry.ExplicitAddress != "" {
			mode = "entry"
		} else {
			current := append(slices.Clone(u.CDNPreferred.V[p.ID]), personalCDNAutomatic(pool, port, cdnScheme(p), time.Now())...)
			if slices.ContainsFunc(selected, func(a string) bool { return slices.Contains(current, a) }) {
				mode = "personal"
			}
		}
		if !available {
			mode = "unavailable"
		}
		rows := []personalCDNPreviewRow{}
		seen := map[string]bool{}
		add := func(address, source string) {
			if seen[address] {
				return
			}
			seen[address] = true
			reason := "not_selected"
			chosen := slices.Contains(selected, address)
			target := entry.Target
			target.Server = address
			switch {
			case !available:
				reason = "account_unavailable"
			case chosen:
				reason = "selected"
				if mode == "public" && source == "automatic" {
					source = "public"
					reason = "selected_public_fallback"
				}
			case slices.Contains(pool.Disabled, address):
				reason = "disabled"
			case cdnAddressBlocked(p, address):
				reason = "public_blocked"
			case personalCDNFailure(pool, entry.EndpointID, target, entry.Transport, entry.Revision) != nil:
				reason = "check_failed"
			case cdnAddressFailureAt(p, entry.EndpointID, target, entry.Transport, entry.Revision) != nil:
				reason = "public_check_failed"
			case len(usableCDNIPsAt(p, []string{address}, port)) == 0:
				reason = "node_check_failed"
			case mode == "entry":
				reason = "entry_override"
			case source == "automatic" && !slices.Contains(personalCDNAutomatic(pool, port, cdnScheme(p), time.Now()), address):
				reason = "automatic_expired_or_protocol_changed"
			case mode == "personal" && source == "public":
				reason = "personal_preferred"
			case mode == "public" && source == "public" && len(selected) > 0:
				reason = "top_n"
			}
			rows = append(rows, personalCDNPreviewRow{Address: address, Source: source, Selected: chosen, Reason: reason})
		}
		for _, a := range u.CDNPreferred.V[p.ID] {
			add(a, "manual")
		}
		for _, a := range pool.Automatic[strconv.Itoa(port)] {
			add(a, "automatic")
		}
		// Include the exact public audience and retained list, plus all selected fallbacks.
		reports, _ := s.cdnAudienceReports(p, port, carrier, region)
		public := s.cdnAddressesForPort(p, port)
		if len(reports) > 0 {
			public = withPinned(mergeCDNReports(reports), p.PinnedHosts.V)
		}
		for _, a := range public {
			add(a, "public")
		}
		for _, a := range selected {
			add(a, "entry")
		}
		result = append(result, personalCDNPreviewEntry{EndpointID: entry.EndpointID, Label: entry.Label, Target: entry.Target, Mode: mode, Selected: selected, Rows: rows})
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"entries": result, "note": "预览入口生成的地址，客户端格式、设备限制及订阅处理规则可能进一步筛选。"})
}
