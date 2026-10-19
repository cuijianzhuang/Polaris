package panel

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// trafficInbox keeps acknowledged, unsettled traffic in the database
// (store.TrafficInbox), so it survives restarts and is shared by every
// panel instance on the same database.
type trafficInbox struct{ db *gorm.DB }

// Add adds traffic to the inbox in its own transaction.
func (b *trafficInbox) Add(nodeID string, ds []model.TrafficDelta) error {
	return b.db.Transaction(func(tx *gorm.DB) error { return addInbox(tx, nodeID, ds) })
}

// addInbox adds traffic to the inbox inside tx.
func addInbox(tx *gorm.DB, nodeID string, ds []model.TrafficDelta) error {
	for _, d := range model.MergeDeltas(ds) {
		if d.Up == 0 && d.Down == 0 {
			continue
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "node_id"}, {Name: "user_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"up": gorm.Expr("traffic_inboxes.up + ?", d.Up), "down": gorm.Expr("traffic_inboxes.down + ?", d.Down),
			}),
		}).Create(&store.TrafficInbox{NodeID: nodeID, UserID: d.UserID, Up: d.Up, Down: d.Down}).Error; err != nil {
			return err
		}
	}
	return nil
}

// Drain reads node -> user -> [up, down] without removing anything; the
// settlement takes exactly these amounts out with takeInbox.
func (b *trafficInbox) Drain() (map[string]map[string][2]int64, error) {
	var rows []store.TrafficInbox
	if err := b.db.Where("up > 0 OR down > 0").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]map[string][2]int64{}
	for _, r := range rows {
		m := out[r.NodeID]
		if m == nil {
			m = map[string][2]int64{}
			out[r.NodeID] = m
		}
		m[r.UserID] = [2]int64{r.Up, r.Down}
	}
	return out, nil
}

// errInboxTaken means another settlement already took part of a snapshot.
var errInboxTaken = errors.New("traffic inbox changed by another settlement")

// takeInbox subtracts a Drain snapshot inside the settlement transaction.
// Traffic added since the snapshot stays. If a row no longer holds its
// snapshot amount, another panel instance settled it first: the whole
// transaction must roll back.
func takeInbox(tx *gorm.DB, data map[string]map[string][2]int64) error {
	for nodeID, users := range data {
		for uid, v := range users {
			r := tx.Model(&store.TrafficInbox{}).
				Where("node_id = ? AND user_id = ? AND up >= ? AND down >= ?", nodeID, uid, v[0], v[1]).
				Updates(map[string]any{"up": gorm.Expr("up - ?", v[0]), "down": gorm.Expr("down - ?", v[1])})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected == 0 {
				return errInboxTaken
			}
		}
	}
	return tx.Where("up = 0 AND down = 0").Delete(&store.TrafficInbox{}).Error
}

// ingestTraffic records an agent's batches and their traffic in one
// transaction and returns the accepted batch IDs (including repeats).
// Batches with a sequence number are deduplicated by the per-queue cursor,
// older agents' batches by ID.
func (s *Server) ingestTraffic(nodeID string, batches []*polarisv1.TrafficBatch, allowed map[string]bool) (accepted []string, dup int, err error) {
	var legacy []*polarisv1.TrafficBatch
	byEpoch := map[string][]*polarisv1.TrafficBatch{}
	for _, b := range batches {
		switch {
		case b.BatchId == "":
		case b.Seq > 0 && b.Epoch != "":
			byEpoch[b.Epoch] = append(byEpoch[b.Epoch], b)
		default:
			legacy = append(legacy, b)
		}
	}
	deltas := func(b *polarisv1.TrafficBatch) []model.TrafficDelta {
		ds := make([]model.TrafficDelta, 0, len(b.Users))
		for _, u := range b.Users {
			if !allowed[u.UserId] && !isTagCounter(u.UserId) {
				continue // not a user of this node
			}
			ds = append(ds, model.TrafficDelta{UserID: u.UserId, Up: max(u.Up, 0), Down: max(u.Down, 0)})
		}
		return ds
	}
	now := time.Now()
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		dup = 0
		var all []model.TrafficDelta
		for epoch, bs := range byEpoch {
			sort.Slice(bs, func(i, j int) bool { return bs[i].Seq < bs[j].Seq })
			var cur store.TrafficCursor
			found := true
			if err := tx.Where("node_id = ? AND epoch = ?", nodeID, epoch).Take(&cur).Error; errors.Is(err, gorm.ErrRecordNotFound) {
				found = false
			} else if err != nil {
				return err
			}
			last := cur.LastSeq
			for _, b := range bs {
				seq := int64(b.Seq)
				if seq <= last {
					dup++
					continue
				}
				last = seq
				all = append(all, deltas(b)...)
			}
			if last == cur.LastSeq && found {
				continue
			}
			// Conditional writes: a concurrent request for the same queue
			// that got here first makes this one fail and retry.
			if !found {
				r := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&store.TrafficCursor{NodeID: nodeID, Epoch: epoch, LastSeq: last, UpdatedAt: now})
				if r.Error != nil {
					return r.Error
				}
				if r.RowsAffected == 0 {
					return errors.New("traffic cursor created concurrently")
				}
				continue
			}
			r := tx.Model(&store.TrafficCursor{}).Where("node_id = ? AND epoch = ? AND last_seq = ?", nodeID, epoch, cur.LastSeq).
				Updates(map[string]any{"last_seq": last, "updated_at": now})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected == 0 {
				return errors.New("traffic cursor moved concurrently")
			}
		}
		for _, b := range legacy {
			r := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&store.TrafficBatchSeen{NodeID: nodeID, BatchID: b.BatchId, CreatedAt: now})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected == 0 {
				dup++
				continue
			}
			all = append(all, deltas(b)...)
		}
		return addInbox(tx, nodeID, all)
	})
	if err != nil {
		return nil, 0, err
	}
	for _, b := range batches {
		accepted = append(accepted, b.BatchId)
	}
	return accepted, dup, nil
}

// settleTraffic moves buffered traffic into users and traffic_hourly, applying
// each node's traffic multiplier, and marks active users online.
func (s *Server) settleTraffic() error {
	data, err := s.traffic.Drain()
	if err != nil || len(data) == 0 {
		return err
	}
	now := time.Now()
	hour := now.UTC().Truncate(time.Hour)
	day := now.Format("2006-01-02")
	rates := map[string]float64{}
	var nodes []store.Node
	s.st.DB.Select("id", "traffic_rate").Find(&nodes)
	for _, n := range nodes {
		rates[n.ID] = n.TrafficRate
	}
	perUser := map[string][2]int64{}
	quotas := s.loadNodeQuotas(data)
	// Plans may carry their own node rates (professional edition).
	prates := s.planRates()
	userPlan := map[string]string{}
	if len(prates) > 0 {
		var ids []string
		for _, users := range data {
			for uid := range users {
				if !isTagCounter(uid) {
					ids = append(ids, uid)
				}
			}
		}
		var us []store.User
		s.st.DB.Select("id", "plan_id").Where("id IN ? AND plan_id <> ''", ids).Find(&us)
		for _, u := range us {
			userPlan[u.ID] = u.PlanID
		}
	}
	var quotaNodes []string // nodes where a user hit a cap or ran out of a pack
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := takeInbox(tx, data); err != nil {
			return err
		}
		for nodeID, users := range data {
			rate := rates[nodeID]
			if rate <= 0 {
				rate = 1
			}
			for uid, v := range users {
				if isTagCounter(uid) {
					// Inbound / outbound totals: raw bytes, no multiplier.
					kind, tag, _ := strings.Cut(uid, ">>>")
					if err := tx.Clauses(clause.OnConflict{
						Columns: []clause.Column{{Name: "node_id"}, {Name: "kind"}, {Name: "tag"}},
						DoUpdates: clause.Assignments(map[string]any{
							"up": gorm.Expr("tag_traffics.up + ?", v[0]), "down": gorm.Expr("tag_traffics.down + ?", v[1]), "updated_at": now,
						}),
					}).Create(&store.TagTraffic{NodeID: nodeID, Kind: kind, Tag: tag, Up: v[0], Down: v[1], UpdatedAt: now}).Error; err != nil {
						return err
					}
					if err := tx.Clauses(clause.OnConflict{
						Columns: []clause.Column{{Name: "node_id"}, {Name: "kind"}, {Name: "tag"}, {Name: "day"}},
						DoUpdates: clause.Assignments(map[string]any{
							"up": gorm.Expr("tag_traffic_dailies.up + ?", v[0]), "down": gorm.Expr("tag_traffic_dailies.down + ?", v[1]),
						}),
					}).Create(&store.TagTrafficDaily{NodeID: nodeID, Kind: kind, Tag: tag, Day: day, Up: v[0], Down: v[1]}).Error; err != nil {
						return err
					}
					continue
				}
				rate := rate
				if r := prates[userPlan[uid]][nodeID]; r > 0 {
					rate = r
				}
				up := int64(math.Round(float64(v[0]) * rate))
				down := int64(math.Round(float64(v[1]) * rate))
				if up == 0 && down == 0 {
					continue
				}
				cu, cd := up, down // charged to the account quota
				if q := quotas[uid+"|"+nodeID]; q != nil {
					var flipped bool
					cu, cd, flipped = chargeNodeQuota(q, up, down)
					if err := tx.Model(&store.UserNodeQuota{}).Where("user_id = ? AND node_id = ?", uid, nodeID).
						Updates(map[string]any{"used": q.Used, "extra_used": q.ExtraUsed, "updated_at": now}).Error; err != nil {
						return err
					}
					if flipped {
						quotaNodes = append(quotaNodes, nodeID)
					}
				}
				cur := perUser[uid]
				cur[0] += cu
				cur[1] += cd
				perUser[uid] = cur
				if err := tx.Clauses(clause.OnConflict{
					Columns: []clause.Column{{Name: "user_id"}, {Name: "node_id"}, {Name: "hour"}},
					DoUpdates: clause.Assignments(map[string]any{
						"up": gorm.Expr("traffic_hourlies.up + ?", up), "down": gorm.Expr("traffic_hourlies.down + ?", down),
						"raw_recorded": true,
						"raw_up":       gorm.Expr("traffic_hourlies.raw_up + ?", v[0]), "raw_down": gorm.Expr("traffic_hourlies.raw_down + ?", v[1]),
						"recorded_up": gorm.Expr("traffic_hourlies.recorded_up + ?", up), "recorded_down": gorm.Expr("traffic_hourlies.recorded_down + ?", down),
						"account_up": gorm.Expr("traffic_hourlies.account_up + ?", cu), "account_down": gorm.Expr("traffic_hourlies.account_down + ?", cd),
						"rate_min": gorm.Expr("CASE WHEN traffic_hourlies.raw_recorded = ? OR traffic_hourlies.rate_min > ? THEN ? ELSE traffic_hourlies.rate_min END", false, rate, rate),
						"rate_max": gorm.Expr("CASE WHEN traffic_hourlies.raw_recorded = ? OR traffic_hourlies.rate_max < ? THEN ? ELSE traffic_hourlies.rate_max END", false, rate, rate),
					}),
				}).Create(&store.TrafficHourly{UserID: uid, NodeID: nodeID, Hour: hour, Up: up, Down: down,
					RawRecorded: true, RawUp: v[0], RawDown: v[1], RecordedUp: up, RecordedDown: down, AccountUp: cu, AccountDown: cd, RateMin: rate, RateMax: rate}).Error; err != nil {
					return err
				}
				if err := tx.Clauses(clause.OnConflict{
					Columns: []clause.Column{{Name: "user_id"}, {Name: "node_id"}, {Name: "day"}},
					DoUpdates: clause.Assignments(map[string]any{
						"up": gorm.Expr("traffic_dailies.up + ?", up), "down": gorm.Expr("traffic_dailies.down + ?", down),
					}),
				}).Create(&store.TrafficDaily{UserID: uid, NodeID: nodeID, Day: day, Up: up, Down: down}).Error; err != nil {
					return err
				}
			}
		}
		for uid, v := range perUser {
			if err := tx.Model(&store.User{}).Where("id = ?", uid).Updates(map[string]any{
				"used_up": gorm.Expr("used_up + ?", v[0]), "used_down": gorm.Expr("used_down + ?", v[1]), "online_at": now,
			}).Error; err != nil {
				return err
			}
		}
		// Start "valid N days from first use" periods.
		if len(perUser) > 0 {
			ids := make([]string, 0, len(perUser))
			for uid := range perUser {
				ids = append(ids, uid)
			}
			var fresh []store.User
			tx.Where("id IN ? AND expire_at IS NULL AND expire_after_days > 0", ids).Find(&fresh)
			for _, u := range fresh {
				exp := now.AddDate(0, 0, u.ExpireAfterDays)
				if err := tx.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"expire_at": exp, "expire_after_days": 0}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if errors.Is(err, errInboxTaken) {
		return nil // another instance settled it; the rest waits for the next round
	}
	if err != nil {
		return err // the traffic stays in the inbox for the next settlement
	}
	if len(quotaNodes) > 0 {
		s.MarkDirty("node quota reached", quotaNodes...)
	}
	s.enforceLimits()
	return nil
}

// enforceLimits moves users between active / limited / expired and lifts
// temporary IP bans, rebuilding the specs of every affected node.
func (s *Server) enforceLimits() {
	now := time.Now()
	var changed []string
	var users []store.User
	s.st.DB.Where("status IN ? AND is_service = ?", []string{"active", "limited", "expired"}, false).Find(&users)
	for _, u := range users {
		if s.autoRenew(&u, now) || s.planFallback(&u, now) {
			changed = append(changed, u.ID)
		}
		want := computeStatus(&u, now)
		if want != u.Status {
			s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("status", want)
			changed = append(changed, u.ID)
			s.audit("system", "user.status", u.Username, u.Status+" -> "+want, "")
			switch want {
			case "limited":
				s.notify(Event{Kind: "user_limited", UserID: u.ID, Text: "⛔ 用户 " + u.Username + " 流量已用尽，已暂停服务"})
			case "expired":
				s.notify(Event{Kind: "user_expired", UserID: u.ID, Text: "⌛ 用户 " + u.Username + " 已到期，已暂停服务"})
			case "active":
				s.notify(Event{Kind: "user_active", UserID: u.ID, Text: "✅ 用户 " + u.Username + " 已恢复服务"})
			}
		}
	}
	var unban []store.User
	s.st.DB.Where("banned_until IS NOT NULL AND banned_until < ?", now).Find(&unban)
	for _, u := range unban {
		s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("banned_until", nil)
		changed = append(changed, u.ID)
	}
	if len(changed) > 0 {
		s.MarkDirty("user status changed", s.nodesForUsers(changed...)...)
	}
}

// autoRenew extends an expired account with a renewal plan: expiry moves on
// by RenewDays (past now) and traffic starts over. It reports a change.
func (s *Server) autoRenew(u *store.User, now time.Time) bool {
	if u.RenewDays <= 0 || u.ExpireAt == nil || !now.After(*u.ExpireAt) || (u.RenewMax > 0 && u.RenewCount >= u.RenewMax) {
		return false
	}
	exp := *u.ExpireAt
	for !exp.After(now) {
		exp = exp.AddDate(0, 0, u.RenewDays)
	}
	u.ExpireAt, u.UsedUp, u.UsedDown, u.RenewCount, u.LastResetAt = &exp, 0, 0, u.RenewCount+1, &now
	u.WarnedUsage, u.WarnedExpire = false, nil
	u.TrafficLimit, u.ExtraTraffic = packLimit(u.TrafficLimit, u.ExtraTraffic), 0
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"expire_at": exp, "used_up": 0, "used_down": 0,
		"renew_count": u.RenewCount, "last_reset_at": now, "warned_usage": false, "warned_expire": nil,
		"traffic_limit": u.TrafficLimit, "extra_traffic": 0})
	s.resetNodeUsage(u.ID)
	s.audit("system", "user.renew", u.Username, fmt.Sprintf("+%d 天，至 %s（第 %d 次）", u.RenewDays, exp.Format("2006-01-02"), u.RenewCount), "")
	s.notify(Event{Kind: "user_renewed", UserID: u.ID, Text: "🔄 用户 " + u.Username + " 已自动续期至 " + exp.Format("2006-01-02")})
	return true
}

// weeklyBoundary is the most recent start (00:00) of the given ISO weekday
// (1 = Monday … 7 = Sunday; others mean Monday).
func weeklyBoundary(now time.Time, weekday int) time.Time {
	if weekday < 1 || weekday > 7 {
		weekday = 1
	}
	want := time.Weekday(weekday % 7)
	d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for d.Weekday() != want {
		d = d.AddDate(0, 0, -1)
	}
	return d
}

// computeStatus derives a user's status from quota and expiry. Disabled is
// sticky and only changed by an admin.
func computeStatus(u *store.User, now time.Time) string {
	if u.Status == "disabled" {
		return "disabled"
	}
	if u.ExpireAt != nil && now.After(*u.ExpireAt) {
		return "expired"
	}
	if u.TrafficLimit > 0 && u.Used() >= u.TrafficLimit {
		return "limited"
	}
	return "active"
}

// resetTrafficCycles applies monthly / rolling reset policies.
func (s *Server) resetTrafficCycles() {
	now := time.Now()
	var users []store.User
	s.st.DB.Where("reset_policy IN ?", []string{"daily", "weekly", "monthly", "cycle"}).Find(&users)
	var changed []string
	for _, u := range users {
		last := u.CreatedAt
		if u.LastResetAt != nil {
			last = *u.LastResetAt
		}
		due := false
		switch u.ResetPolicy {
		case "daily":
			due = last.Before(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()))
		case "weekly":
			due = last.Before(weeklyBoundary(now, u.ResetDay))
		case "monthly":
			boundary := monthlyBoundary(now, u.ResetDay, 0)
			if now.Before(boundary) {
				boundary = monthlyBoundary(now, u.ResetDay, -1)
			}
			due = last.Before(boundary)
		case "cycle":
			days := u.ResetDay
			if days < 1 {
				days = 30
			}
			due = now.Sub(last) >= time.Duration(days)*24*time.Hour
		}
		if due {
			// A new period: traffic packs bought for the last one expire.
			s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"used_up": 0, "used_down": 0, "last_reset_at": now,
				"traffic_limit": packLimit(u.TrafficLimit, u.ExtraTraffic), "extra_traffic": 0})
			s.resetNodeUsage(u.ID)
			s.audit("system", "user.reset_traffic", u.Username, u.ResetPolicy, "")
			if u.Status == "limited" {
				changed = append(changed, u.ID)
			}
		}
	}
	if len(changed) > 0 {
		s.enforceLimits()
	}
}

// handleOnlineIPs records source IPs and enforces ip_limit.
func (s *Server) handleOnlineIPs(nodeID string, m *polarisv1.OnlineIPs) {
	banMinutes, _ := strconv.Atoi(s.st.GetSetting("ip_ban_minutes", "0"))
	trusted := parseNetList(s.st.GetSetting("ip_limit_trusted", ""))
	allowed := s.nodeUsers(nodeID)
	for _, u := range m.Users {
		if !allowed[u.UserId] {
			continue // not a user of this node
		}
		list := s.shared.setNodeIPs(u.UserId, nodeID, u.Ips, time.Now())
		sort.Strings(list)
		counted := 0
		for _, ip := range list {
			if !trusted.contains(ip) {
				counted++
			}
		}
		var user store.User
		if err := s.st.DB.First(&user, "id = ?", u.UserId).Error; err != nil {
			continue
		}
		s.st.DB.Model(&store.User{}).Where("id = ?", user.ID).Updates(map[string]any{"online_ips": store.NewJSON(list), "online_at": time.Now()})
		if user.IPLimit > 0 && counted > user.IPLimit {
			if s.alertOnce("ip:"+user.ID, 10*time.Minute) {
				s.audit("system", "user.ip_limit", user.Username, fmt.Sprintf("%d IPs > limit %d: %s", counted, user.IPLimit, strings.Join(list, ", ")), "")
				s.notify(Event{Kind: "ip_limit", UserID: user.ID, Text: fmt.Sprintf("🚨 用户 %s 同时在线 IP %d 个（不含受信任地址），超过限制 %d", user.Username, counted, user.IPLimit)})
			}
			if banMinutes > 0 && user.BannedUntil == nil {
				until := time.Now().Add(time.Duration(banMinutes) * time.Minute)
				s.st.DB.Model(&store.User{}).Where("id = ?", user.ID).Update("banned_until", until)
				s.MarkDirty("ip limit ban", s.nodesForUsers(user.ID)...)
			}
		}
	}
}

// alertOnce reports whether an alert for key may fire now (once per every,
// across panel instances when Redis is configured).
func (s *Server) alertOnce(key string, every time.Duration) bool {
	return s.shared.once(key, every)
}

// onlineIPWindow is how long a reported source IP counts as online. Agents
// report every 20 seconds and only list users that have connections, so an
// IP that stops being reported is gone after three missed reports.
const onlineIPWindow = 75 * time.Second

// monthlyBoundary is the reset instant for "day of month" in the month
// offset from now's; days past the month's end mean its last day.
func monthlyBoundary(now time.Time, day, offset int) time.Time {
	if day < 1 {
		day = 1
	}
	first := time.Date(now.Year(), now.Month()+time.Month(offset), 1, 0, 0, 0, 0, now.Location())
	if last := first.AddDate(0, 1, -1).Day(); day > last {
		day = last
	}
	return first.AddDate(0, 0, day-1)
}

// netList is a set of IPs / CIDRs (trusted addresses exempt from ip_limit,
// e.g. a home or office egress shared by several devices).
type netList []netip.Prefix

func parseNetList(s string) netList {
	var out netList
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == '\r' }) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
		} else if a, err := netip.ParseAddr(f); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}

func (l netList) contains(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range l {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// isTagCounter reports a per-inbound / per-outbound counter in a traffic
// delta (see core.TagPrefixInbound).
func isTagCounter(id string) bool {
	return strings.HasPrefix(id, core.TagPrefixInbound) || strings.HasPrefix(id, core.TagPrefixOutbound)
}

// realUsers keeps the rows of real users: chain service accounts carry
// traffic another node already counted for its user (a hop through this
// node), so totals and rankings would count it twice. Per-node views keep
// them, since the hop is load on that node.
func realUsers(q *gorm.DB) *gorm.DB {
	return q.Where("user_id NOT IN (SELECT id FROM users WHERE is_service = ?)", true)
}

// packLimit is a quota without the traffic packs that expire with the
// period (never below what is left without them, nor unlimited by mistake).
func packLimit(limit, extra int64) int64 {
	if extra <= 0 || limit <= extra {
		return limit
	}
	return limit - extra
}
