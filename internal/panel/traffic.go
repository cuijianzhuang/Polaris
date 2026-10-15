package panel

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// TrafficBuffer accumulates reported traffic between settlements.
type TrafficBuffer interface {
	Add(nodeID string, ds []model.TrafficDelta) error
	// Drain returns and clears node -> user -> [up, down].
	Drain() (map[string]map[string][2]int64, error)
	// Seen records a batch id and reports whether it was already processed.
	Seen(batchID string) bool
	// Forget drops a recorded batch id (its Add failed; the retry must count).
	Forget(batchID string)
}

type memBuffer struct {
	mu    sync.Mutex
	data  map[string]map[string][2]int64
	seen  map[string]time.Time
	clean time.Time
}

func newMemBuffer() *memBuffer {
	return &memBuffer{data: map[string]map[string][2]int64{}, seen: map[string]time.Time{}}
}

func (b *memBuffer) Add(nodeID string, ds []model.TrafficDelta) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.data[nodeID]
	if m == nil {
		m = map[string][2]int64{}
		b.data[nodeID] = m
	}
	for _, d := range ds {
		v := m[d.UserID]
		v[0] += d.Up
		v[1] += d.Down
		m[d.UserID] = v
	}
	return nil
}

func (b *memBuffer) Drain() (map[string]map[string][2]int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.data
	b.data = map[string]map[string][2]int64{}
	return out, nil
}

func (b *memBuffer) Seen(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Sub(b.clean) > 10*time.Minute {
		for k, t := range b.seen {
			if now.Sub(t) > time.Hour {
				delete(b.seen, k)
			}
		}
		b.clean = now
	}
	if _, ok := b.seen[id]; ok {
		return true
	}
	b.seen[id] = now
	return false
}

func (b *memBuffer) Forget(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.seen, id)
}

// redisBuffer uses HINCRBY so several panel replicas can share counters.
type redisBuffer struct{ c *redis.Client }

func newRedisBuffer(url string) (*redisBuffer, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	c := redis.NewClient(opt)
	if err := c.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	return &redisBuffer{c: c}, nil
}

func (b *redisBuffer) Add(nodeID string, ds []model.TrafficDelta) error {
	ctx := context.Background()
	p := b.c.TxPipeline()
	key := "polaris:traffic:" + nodeID
	p.SAdd(ctx, "polaris:traffic:nodes", nodeID)
	for _, d := range ds {
		if d.Up != 0 {
			p.HIncrBy(ctx, key, d.UserID+":up", d.Up)
		}
		if d.Down != 0 {
			p.HIncrBy(ctx, key, d.UserID+":down", d.Down)
		}
	}
	_, err := p.Exec(ctx)
	return err
}

func (b *redisBuffer) Drain() (map[string]map[string][2]int64, error) {
	ctx := context.Background()
	nodes, err := b.c.SMembers(ctx, "polaris:traffic:nodes").Result()
	if err != nil {
		return nil, err
	}
	out := map[string]map[string][2]int64{}
	for _, n := range nodes {
		key := "polaris:traffic:" + n
		snap := key + ":drain:" + strconv.FormatInt(time.Now().UnixNano(), 10)
		if err := b.c.Rename(ctx, key, snap).Err(); err != nil {
			continue // key vanished: nothing to drain
		}
		vals, err := b.c.HGetAll(ctx, snap).Result()
		if err != nil {
			// Keep what was already drained; this node's snapshot is merged
			// back so the next settlement picks it up.
			_ = b.c.Eval(ctx, mergeBackLua, []string{snap, key}).Err()
			continue
		}
		b.c.Del(ctx, snap)
		m := map[string][2]int64{}
		for k, v := range vals {
			i := strings.LastIndex(k, ":")
			if i < 0 {
				continue
			}
			n, _ := strconv.ParseInt(v, 10, 64)
			cur := m[k[:i]]
			if k[i+1:] == "up" {
				cur[0] += n
			} else {
				cur[1] += n
			}
			m[k[:i]] = cur
		}
		out[n] = m
	}
	return out, nil
}

func (b *redisBuffer) Seen(id string) bool {
	ok, err := b.c.SetNX(context.Background(), "polaris:batch:"+id, 1, time.Hour).Result()
	return err == nil && !ok
}

func (b *redisBuffer) Forget(id string) {
	b.c.Del(context.Background(), "polaris:batch:"+id)
}

// mergeBackLua adds a drain snapshot back into the live counters and deletes it.
const mergeBackLua = `local v = redis.call('HGETALL', KEYS[1])
for i = 1, #v, 2 do redis.call('HINCRBY', KEYS[2], v[i], v[i+1]) end
redis.call('DEL', KEYS[1])
return 1`

// settleTraffic moves buffered traffic into users and traffic_hourly, applying
// each node's traffic multiplier, and marks active users online.
func (s *Server) settleTraffic() error {
	data, err := s.traffic.Drain()
	if err != nil || len(data) == 0 {
		return err
	}
	now := time.Now()
	hour := now.UTC().Truncate(time.Hour)
	rates := map[string]float64{}
	var nodes []store.Node
	s.st.DB.Select("id", "traffic_rate").Find(&nodes)
	for _, n := range nodes {
		rates[n.ID] = n.TrafficRate
	}
	perUser := map[string][2]int64{}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		for nodeID, users := range data {
			rate := rates[nodeID]
			if rate <= 0 {
				rate = 1
			}
			for uid, v := range users {
				up := int64(math.Round(float64(v[0]) * rate))
				down := int64(math.Round(float64(v[1]) * rate))
				if up == 0 && down == 0 {
					continue
				}
				cur := perUser[uid]
				cur[0] += up
				cur[1] += down
				perUser[uid] = cur
				if err := tx.Clauses(clause.OnConflict{
					Columns: []clause.Column{{Name: "user_id"}, {Name: "node_id"}, {Name: "hour"}},
					DoUpdates: clause.Assignments(map[string]any{
						"up": gorm.Expr("traffic_hourlies.up + ?", up), "down": gorm.Expr("traffic_hourlies.down + ?", down),
					}),
				}).Create(&store.TrafficHourly{UserID: uid, NodeID: nodeID, Hour: hour, Up: up, Down: down}).Error; err != nil {
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
	if err != nil {
		// Put the traffic back so the next settlement counts it.
		for nodeID, users := range data {
			ds := make([]model.TrafficDelta, 0, len(users))
			for uid, v := range users {
				ds = append(ds, model.TrafficDelta{UserID: uid, Up: v[0], Down: v[1]})
			}
			_ = s.traffic.Add(nodeID, ds)
		}
		return err
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
	s.st.DB.Where("reset_policy IN ?", []string{"monthly", "cycle"}).Find(&users)
	var changed []string
	for _, u := range users {
		last := u.CreatedAt
		if u.LastResetAt != nil {
			last = *u.LastResetAt
		}
		due := false
		switch u.ResetPolicy {
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
			s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"used_up": 0, "used_down": 0, "last_reset_at": now})
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
	allowed := s.nodeUsers(nodeID)
	for _, u := range m.Users {
		if !allowed[u.UserId] {
			continue // not a user of this node
		}
		s.onlineMu.Lock()
		cur := s.onlineIPs[u.UserId]
		if cur == nil {
			cur = map[string]map[string]time.Time{}
			s.onlineIPs[u.UserId] = cur
		}
		ips := map[string]time.Time{}
		for _, ip := range u.Ips {
			ips[ip] = time.Now()
		}
		cur[nodeID] = ips
		all := map[string]bool{}
		for n, perNode := range cur {
			for ip, seen := range perNode {
				if time.Since(seen) >= onlineIPWindow {
					delete(perNode, ip) // the node stopped reporting it
					continue
				}
				all[ip] = true
			}
			if len(perNode) == 0 {
				delete(cur, n)
			}
		}
		s.onlineMu.Unlock()
		list := make([]string, 0, len(all))
		for ip := range all {
			list = append(list, ip)
		}
		var user store.User
		if err := s.st.DB.First(&user, "id = ?", u.UserId).Error; err != nil {
			continue
		}
		s.st.DB.Model(&store.User{}).Where("id = ?", user.ID).Updates(map[string]any{"online_ips": store.NewJSON(list), "online_at": time.Now()})
		if user.IPLimit > 0 && len(list) > user.IPLimit {
			if s.alertOnce("ip:"+user.ID, 10*time.Minute) {
				s.audit("system", "user.ip_limit", user.Username, fmt.Sprintf("%d IPs > limit %d: %s", len(list), user.IPLimit, strings.Join(list, ", ")), "")
				s.notify(Event{Kind: "ip_limit", UserID: user.ID, Text: fmt.Sprintf("🚨 用户 %s 同时在线 IP %d 个，超过限制 %d", user.Username, len(list), user.IPLimit)})
			}
			if banMinutes > 0 && user.BannedUntil == nil {
				until := time.Now().Add(time.Duration(banMinutes) * time.Minute)
				s.st.DB.Model(&store.User{}).Where("id = ?", user.ID).Update("banned_until", until)
				s.MarkDirty("ip limit ban", s.nodesForUsers(user.ID)...)
			}
		}
	}
}

func (s *Server) alertOnce(key string, every time.Duration) bool {
	s.onlineMu.Lock()
	defer s.onlineMu.Unlock()
	if t, ok := s.alerts[key]; ok && time.Since(t) < every {
		return false
	}
	s.alerts[key] = time.Now()
	return true
}

// onlineIPWindow is how long a reported source IP counts as online. Agents
// report every minute and only list users that have connections.
const onlineIPWindow = 3 * time.Minute

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
