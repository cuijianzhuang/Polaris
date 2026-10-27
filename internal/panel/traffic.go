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
	var rows []store.TrafficInbox
	for _, d := range model.MergeDeltas(ds) { // one row per user: "excluded" is its amount
		if d.Up == 0 && d.Down == 0 {
			continue
		}
		rows = append(rows, store.TrafficInbox{NodeID: nodeID, UserID: d.UserID, Up: d.Up, Down: d.Down})
	}
	return upsertAll(tx, rows, clause.OnConflict{
		Columns: []clause.Column{{Name: "node_id"}, {Name: "user_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"up": gorm.Expr("traffic_inboxes.up + excluded.up"), "down": gorm.Expr("traffic_inboxes.down + excluded.down"),
		}),
	})
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
	for _, nodeID := range sortedKeys(data) {
		users := data[nodeID]
		ids := sortedKeys(users)
		for len(ids) > 0 {
			n := min(len(ids), settleBatch)
			chunk := ids[:n]
			ids = ids[n:]
			vals, args := valuesList(chunk, func(id string) []int64 { v := users[id]; return v[:] })
			r := tx.Exec("UPDATE traffic_inboxes AS t SET up = t.up - v.column2, down = t.down - v.column3 FROM (VALUES "+vals+") AS v"+
				" WHERE t.node_id = ? AND t.user_id = v.column1 AND t.up >= v.column2 AND t.down >= v.column3", append(args, nodeID)...)
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != int64(len(chunk)) {
				return errInboxTaken // some row no longer holds its snapshot amount
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
			if model.IsDiagnosticCounter(u.UserId) {
				continue
			}
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
	fwdOwners := s.forwardOwners(data)
	fwdAdd := map[string]int64{} // user -> forwarded bytes charged
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := takeInbox(tx, data); err != nil {
			return err
		}
		// Work out every row first, then write each table in a few batched
		// statements instead of several statements per user and node.
		var (
			tags     []store.TagTraffic
			tagDays  []store.TagTrafficDaily
			hourlies []store.TrafficHourly
			dailies  []store.TrafficDaily
		)
		for _, nodeID := range sortedKeys(data) {
			users := data[nodeID]
			rate := rates[nodeID]
			if rate <= 0 {
				rate = 1
			}
			for _, uid := range sortedKeys(users) {
				v := users[uid]
				if isTagCounter(uid) {
					// Inbound / outbound totals: raw bytes, no multiplier.
					kind, tag, _ := strings.Cut(uid, ">>>")
					tags = append(tags, store.TagTraffic{NodeID: nodeID, Kind: kind, Tag: tag, Up: v[0], Down: v[1], UpdatedAt: now})
					tagDays = append(tagDays, store.TagTrafficDaily{NodeID: nodeID, Kind: kind, Tag: tag, Day: day, Up: v[0], Down: v[1]})
					if o, ok := fwdOwners[tag]; ok && kind == "inbound" {
						// A self-service forward: its own allowance, node rate x plan rate.
						fwdAdd[o.user] += int64(math.Round(float64(v[0]+v[1]) * rate * o.rate))
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
					used0, extra0 := q.Used, q.ExtraUsed
					cu, cd, flipped = chargeNodeQuota(q, up, down)
					// Add the deltas: q was read before this transaction, and a
					// reset made since then must not be overwritten.
					if err := tx.Model(&store.UserNodeQuota{}).Where("user_id = ? AND node_id = ?", uid, nodeID).
						Updates(map[string]any{"used": gorm.Expr("used + ?", q.Used-used0), "extra_used": gorm.Expr("extra_used + ?", q.ExtraUsed-extra0), "updated_at": now}).Error; err != nil {
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
				hourlies = append(hourlies, store.TrafficHourly{UserID: uid, NodeID: nodeID, Hour: hour, Up: up, Down: down,
					RawRecorded: true, RawUp: v[0], RawDown: v[1], RecordedUp: up, RecordedDown: down, AccountUp: cu, AccountDown: cd, RateMin: rate, RateMax: rate})
				dailies = append(dailies, store.TrafficDaily{UserID: uid, NodeID: nodeID, Day: day, Up: up, Down: down})
			}
		}
		// Each conflict key occurs once per batch (data is keyed by node, then
		// user or tag), so "excluded" is this round's amount for that row.
		if err := upsertAll(tx, tags, clause.OnConflict{
			Columns: []clause.Column{{Name: "node_id"}, {Name: "kind"}, {Name: "tag"}},
			DoUpdates: clause.Assignments(map[string]any{
				"up": gorm.Expr("tag_traffics.up + excluded.up"), "down": gorm.Expr("tag_traffics.down + excluded.down"), "updated_at": now,
			}),
		}); err != nil {
			return err
		}
		if err := upsertAll(tx, tagDays, clause.OnConflict{
			Columns: []clause.Column{{Name: "node_id"}, {Name: "kind"}, {Name: "tag"}, {Name: "day"}},
			DoUpdates: clause.Assignments(map[string]any{
				"up": gorm.Expr("tag_traffic_dailies.up + excluded.up"), "down": gorm.Expr("tag_traffic_dailies.down + excluded.down"),
			}),
		}); err != nil {
			return err
		}
		if err := upsertAll(tx, hourlies, clause.OnConflict{
			Columns: []clause.Column{{Name: "user_id"}, {Name: "node_id"}, {Name: "hour"}},
			DoUpdates: clause.Assignments(map[string]any{
				"up": gorm.Expr("traffic_hourlies.up + excluded.up"), "down": gorm.Expr("traffic_hourlies.down + excluded.down"),
				"raw_recorded": true,
				"raw_up":       gorm.Expr("traffic_hourlies.raw_up + excluded.raw_up"), "raw_down": gorm.Expr("traffic_hourlies.raw_down + excluded.raw_down"),
				"recorded_up": gorm.Expr("traffic_hourlies.recorded_up + excluded.recorded_up"), "recorded_down": gorm.Expr("traffic_hourlies.recorded_down + excluded.recorded_down"),
				"account_up": gorm.Expr("traffic_hourlies.account_up + excluded.account_up"), "account_down": gorm.Expr("traffic_hourlies.account_down + excluded.account_down"),
				"rate_min": gorm.Expr("CASE WHEN traffic_hourlies.raw_recorded = ? OR traffic_hourlies.rate_min > excluded.rate_min THEN excluded.rate_min ELSE traffic_hourlies.rate_min END", false),
				"rate_max": gorm.Expr("CASE WHEN traffic_hourlies.raw_recorded = ? OR traffic_hourlies.rate_max < excluded.rate_max THEN excluded.rate_max ELSE traffic_hourlies.rate_max END", false),
			}),
		}); err != nil {
			return err
		}
		if err := upsertAll(tx, dailies, clause.OnConflict{
			Columns: []clause.Column{{Name: "user_id"}, {Name: "node_id"}, {Name: "day"}},
			DoUpdates: clause.Assignments(map[string]any{
				"up": gorm.Expr("traffic_dailies.up + excluded.up"), "down": gorm.Expr("traffic_dailies.down + excluded.down"),
			}),
		}); err != nil {
			return err
		}
		used := make(map[string][]int64, len(perUser))
		for uid, v := range perUser {
			used[uid] = []int64{v[0], v[1]}
		}
		if err := addToUsers(tx, used, []string{"used_up", "used_down"}, map[string]any{"online_at": now}); err != nil {
			return err
		}
		fwd := map[string][]int64{}
		for uid, b := range fwdAdd {
			if b > 0 {
				fwd[uid] = []int64{b}
			}
		}
		if err := addToUsers(tx, fwd, []string{"fwd_used"}, nil); err != nil {
			return err
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
	if len(fwdAdd) > 0 {
		s.capForwards(fwdAdd)
	}
	s.enforceLimits()
	return nil
}

// settleBatch is how many rows one batched statement writes.
const settleBatch = 500

// upsertAll inserts rows in batches, merging into existing rows on conflict.
func upsertAll[T any](tx *gorm.DB, rows []T, onConflict clause.OnConflict) error {
	if len(rows) == 0 {
		return nil
	}
	return tx.Clauses(onConflict).CreateInBatches(rows, settleBatch).Error
}

// addToUsers adds per-user amounts to integer columns of users (vals[id][i]
// goes to cols[i]) and sets set on the same rows, in one UPDATE per batch.
func addToUsers(tx *gorm.DB, vals map[string][]int64, cols []string, set map[string]any) error {
	setSQL, setArgs := "", []any{}
	for _, k := range sortedKeys(set) {
		setSQL += ", " + k + " = ?"
		setArgs = append(setArgs, set[k])
	}
	for i, c := range cols {
		setSQL += fmt.Sprintf(", %s = users.%s + v.column%d", c, c, i+2)
	}
	ids := sortedKeys(vals)
	for len(ids) > 0 {
		n := min(len(ids), settleBatch)
		chunk := ids[:n]
		ids = ids[n:]
		list, args := valuesList(chunk, func(id string) []int64 { return vals[id] })
		r := tx.Exec("UPDATE users SET updated_at = ?"+setSQL+" FROM (VALUES "+list+") AS v WHERE users.id = v.column1",
			append(append([]any{time.Now()}, setArgs...), args...)...)
		if r.Error != nil {
			return r.Error
		}
	}
	return nil
}

// valuesList builds "(?, CAST(? AS BIGINT), ...), ..." for a VALUES list of
// (id, amounts...) rows; the columns are column1 (id), column2, column3... in
// both SQLite and PostgreSQL. The casts keep PostgreSQL from reading the
// amounts as text.
func valuesList(ids []string, amounts func(string) []int64) (string, []any) {
	var sb strings.Builder
	var args []any
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?")
		args = append(args, id)
		for _, a := range amounts(id) {
			sb.WriteString(", CAST(? AS BIGINT)")
			args = append(args, a)
		}
		sb.WriteString(")")
	}
	return sb.String(), args
}

// sortedKeys returns m's keys in order, so batches are written in a stable
// order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type fwdOwner struct {
	user string
	rate float64
}

// forwardOwners maps the traffic tags of self-service forwards in data
// ("fwd-<rule id>") to their owner and plan rate.
func (s *Server) forwardOwners(data map[string]map[string][2]int64) map[string]fwdOwner {
	var ids []string
	for _, users := range data {
		for uid := range users {
			if kind, tag, ok := strings.Cut(uid, ">>>"); ok && kind == "inbound" && strings.HasPrefix(tag, "fwd-") {
				ids = append(ids, strings.TrimPrefix(tag, "fwd-"))
			}
		}
	}
	out := map[string]fwdOwner{}
	if len(ids) == 0 {
		return out
	}
	var rs []store.ForwardRule
	s.st.DB.Select("id", "user_id").Where("id IN ? AND user_id <> ''", ids).Find(&rs)
	if len(rs) == 0 {
		return out
	}
	uids := make([]string, 0, len(rs))
	for _, r := range rs {
		uids = append(uids, r.UserID)
	}
	var us []store.User
	s.st.DB.Select("id", "fwd_rate").Where("id IN ?", uids).Find(&us)
	rates := map[string]float64{}
	for _, u := range us {
		rates[u.ID] = fwdRate(u.ForwardEntitlement)
	}
	for _, r := range rs {
		if rt, ok := rates[r.UserID]; ok {
			out["fwd-"+r.ID] = fwdOwner{user: r.UserID, rate: rt}
		}
	}
	return out
}

// capForwards withholds the forwards of users who just used up their
// forwarding allowance.
func (s *Server) capForwards(charged map[string]int64) {
	ids := make([]string, 0, len(charged))
	for id := range charged {
		ids = append(ids, id)
	}
	var us []store.User
	s.st.DB.Select("id", "username").Where("id IN ? AND fwd_traffic > 0 AND fwd_used >= fwd_traffic AND fwd_capped = ?", ids, false).Find(&us)
	var capped []string
	for _, u := range us {
		if s.st.DB.Model(&store.User{}).Where("id = ? AND fwd_capped = ?", u.ID, false).Update("fwd_capped", true).RowsAffected == 1 {
			capped = append(capped, u.ID)
			s.notify(Event{Kind: "user_forward_capped", UserID: u.ID, Text: "⛔ 用户 " + u.Username + " 的转发流量已用尽，转发已暂停"})
		}
	}
	if len(capped) > 0 {
		s.MarkDirty("forward allowance used up", s.userForwardNodes(capped...)...)
	}
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
			e := Event{UserID: u.ID}
			switch want {
			case "limited":
				e.Kind, e.Text = "user_limited", "⛔ 用户 "+u.Username+" 流量已用尽，已暂停服务"
			case "expired":
				e.Kind, e.Text = "user_expired", "⌛ 用户 "+u.Username+" 已到期，已暂停服务"
			case "active":
				e.Kind, e.Text = "user_active", "✅ 用户 "+u.Username+" 已恢复服务"
			}
			updated := false
			err := s.st.DB.Transaction(func(tx *gorm.DB) error {
				res := tx.Model(&store.User{}).Where("id = ? AND status = ?", u.ID, u.Status).Update("status", want)
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected == 0 {
					return nil
				}
				updated = true
				snapshot := u
				snapshot.Status = want
				e.Data = map[string]any{"user": userBrief(&snapshot)}
				return s.queueBusinessEvent(tx, e)
			})
			if err != nil {
				s.log.Error("user status and event transaction failed", "user", u.ID, "err", err)
				continue
			}
			if !updated {
				continue
			}
			changed = append(changed, u.ID)
			s.audit("system", "user.status", u.Username, u.Status+" -> "+want, "")
			s.notifyCommitted(e)
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
	due := func(u *store.User) bool {
		return u.RenewDays > 0 && u.ExpireAt != nil && now.After(*u.ExpireAt) && (u.RenewMax <= 0 || u.RenewCount < u.RenewMax)
	}
	if !due(u) {
		return false
	}
	var exp time.Time
	renewed := false
	// u was read before; re-read it locked, so a purchase or an admin edit
	// made since is neither lost nor renewed on top of.
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(u, "id = ?", u.ID).Error; err != nil {
			return err
		}
		if !due(u) {
			return nil
		}
		exp = *u.ExpireAt
		for !exp.After(now) {
			exp = exp.AddDate(0, 0, u.RenewDays)
		}
		u.ExpireAt, u.UsedUp, u.UsedDown, u.RenewCount, u.LastResetAt = &exp, 0, 0, u.RenewCount+1, &now
		u.WarnedUsage, u.WarnedExpire = false, nil
		u.TrafficLimit, u.ExtraTraffic = packLimit(u.TrafficLimit, u.ExtraTraffic), 0
		renewed = true
		if err := tx.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"expire_at": exp, "used_up": 0, "used_down": 0,
			"renew_count": u.RenewCount, "last_reset_at": now, "warned_usage": false, "warned_expire": nil,
			"traffic_limit": u.TrafficLimit, "extra_traffic": 0, "fwd_used": 0, "fwd_capped": false}).Error; err != nil {
			return err
		}
		return s.queueBusinessEvent(tx, Event{Kind: "user_renewed", UserID: u.ID, Data: map[string]any{"user": userBrief(u)}, Text: "🔄 用户 " + u.Username + " 已自动续期至 " + exp.Format("2006-01-02")})
	})
	if err != nil || !renewed {
		return false
	}
	s.resetNodeUsage(u.ID)
	s.audit("system", "user.renew", u.Username, fmt.Sprintf("+%d 天，至 %s（第 %d 次）", u.RenewDays, exp.Format("2006-01-02"), u.RenewCount), "")
	s.notifyCommitted(Event{Kind: "user_renewed", UserID: u.ID, Text: "🔄 用户 " + u.Username + " 已自动续期至 " + exp.Format("2006-01-02")})
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
	var changed, cappedNodes []string
	for _, u := range users {
		last := u.CreatedAt
		if u.LastResetAt != nil {
			last = *u.LastResetAt
		}
		due, resetAt := false, now
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
			period := time.Duration(days) * 24 * time.Hour
			due = now.Sub(last) >= period
			if due {
				// Stay on the cycle's boundary: the check runs periodically,
				// and stamping "now" would push every later cycle back.
				resetAt = last.Add(now.Sub(last) / period * period)
			}
		}
		if due {
			// A new period: traffic packs bought for the last one expire.
			s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"used_up": 0, "used_down": 0, "last_reset_at": resetAt,
				"traffic_limit": packLimit(u.TrafficLimit, u.ExtraTraffic), "extra_traffic": 0, "fwd_used": 0, "fwd_capped": false})
			if u.FwdCapped {
				cappedNodes = append(cappedNodes, s.userForwardNodes(u.ID)...) // their forwards come back
			}
			capped, _ := s.nodeAccess(u.ID)
			s.resetNodeUsage(u.ID)
			for nodeID := range capped { // the cap lifts: those nodes take the user back
				cappedNodes = append(cappedNodes, nodeID)
			}
			s.audit("system", "user.reset_traffic", u.Username, u.ResetPolicy, "")
			if u.Status == "limited" {
				changed = append(changed, u.ID)
			}
		}
	}
	if len(cappedNodes) > 0 {
		s.MarkDirty("node quota reset", cappedNodes...)
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
