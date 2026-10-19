package panel

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// sharedState holds short-lived state that several panel instances should
// see alike: online source IPs, rate-limit counters and alert
// de-duplication. Without redis_url each instance keeps its own copy in
// memory (fine for a single panel); with it, the copy lives in Redis.
type sharedState interface {
	// setNodeIPs replaces the IPs one node reports for a user and returns
	// the user's IPs on every node seen within onlineIPWindow.
	setNodeIPs(userID, nodeID string, ips []string, now time.Time) []string
	// onlineIPs returns user -> node -> ip -> last seen.
	onlineIPs(userIDs []string) map[string]map[string]map[string]time.Time
	// allow counts one request for key against perMinute in bucket name.
	allow(name string, perMinute int, key string) bool
	// once reports whether key has not fired within every, and marks it.
	once(key string, every time.Duration) bool
}

// sharedLimiter is a named per-key rate limit on sharedState.
type sharedLimiter struct {
	s         sharedState
	name      string
	perMinute int
}

func (l *sharedLimiter) Allow(key string) bool { return l.s.allow(l.name, l.perMinute, key) }

// --- memory ---

type memState struct {
	mu     sync.Mutex
	ips    map[string]map[string]map[string]time.Time // user -> node -> ip
	alerts map[string]time.Time
	limMu  sync.Mutex
	lims   map[string]*ipLimiter
}

func newMemState() *memState {
	return &memState{ips: map[string]map[string]map[string]time.Time{}, alerts: map[string]time.Time{}, lims: map[string]*ipLimiter{}}
}

func (m *memState) setNodeIPs(userID, nodeID string, ips []string, now time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.ips[userID]
	if cur == nil {
		cur = map[string]map[string]time.Time{}
		m.ips[userID] = cur
	}
	set := map[string]time.Time{}
	for _, ip := range ips {
		set[ip] = now
	}
	cur[nodeID] = set
	all := map[string]bool{}
	for n, perNode := range cur {
		for ip, seen := range perNode {
			if now.Sub(seen) >= onlineIPWindow {
				delete(perNode, ip) // the node stopped reporting it
				continue
			}
			all[ip] = true
		}
		if len(perNode) == 0 {
			delete(cur, n)
		}
	}
	if len(cur) == 0 {
		delete(m.ips, userID)
	}
	list := make([]string, 0, len(all))
	for ip := range all {
		list = append(list, ip)
	}
	return list
}

func (m *memState) onlineIPs(userIDs []string) map[string]map[string]map[string]time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]map[string]map[string]time.Time{}
	for _, u := range userIDs {
		for node, ips := range m.ips[u] {
			if out[u] == nil {
				out[u] = map[string]map[string]time.Time{}
			}
			cp := make(map[string]time.Time, len(ips))
			for ip, at := range ips {
				cp[ip] = at
			}
			out[u][node] = cp
		}
	}
	return out
}

func (m *memState) allow(name string, perMinute int, key string) bool {
	m.limMu.Lock()
	l := m.lims[name]
	if l == nil {
		l = newIPLimiter(perMinute)
		m.lims[name] = l
	}
	m.limMu.Unlock()
	return l.Allow(key)
}

func (m *memState) once(key string, every time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if len(m.alerts) > 10000 {
		for k, t := range m.alerts {
			if now.Sub(t) > 24*time.Hour {
				delete(m.alerts, k)
			}
		}
	}
	if t, ok := m.alerts[key]; ok && now.Sub(t) < every {
		return false
	}
	m.alerts[key] = now
	return true
}

// --- Redis ---

// redisState keeps the shared state in Redis. When Redis fails, it falls
// back to the instance's memory so the panel keeps working (limits and
// online counts are then per instance until Redis is back).
type redisState struct {
	c      *redis.Client
	prefix string // key prefix; tests use their own
	mem    *memState
	log    *slog.Logger
	warn   sync.Mutex
	last   time.Time
}

func newRedisState(url string, log *slog.Logger) (*redisState, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis_url: %w", err)
	}
	c := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		c.Close()
		return nil, fmt.Errorf("redis: %w", err)
	}
	return &redisState{c: c, prefix: "polaris:", mem: newMemState(), log: log}, nil
}

func (r *redisState) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Second)
}

// failed logs a Redis error at most once a minute.
func (r *redisState) failed(err error) {
	r.warn.Lock()
	defer r.warn.Unlock()
	if time.Since(r.last) > time.Minute {
		r.last = time.Now()
		r.log.Warn("redis unavailable; using this instance's memory for online IPs and rate limits", "err", err)
	}
}

// setIPsLua replaces node ARGV[1]'s fields ("node|ip" -> unix ms), drops
// fields older than the window and returns the remaining IPs.
const setIPsLua = `
local key, node, now, window = KEYS[1], ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3])
local cur = redis.call('HGETALL', key)
for i = 1, #cur, 2 do
  local f, t = cur[i], tonumber(cur[i+1])
  if string.sub(f, 1, #node + 1) == node .. '|' or now - t >= window then
    redis.call('HDEL', key, f)
  end
end
for i = 4, #ARGV do redis.call('HSET', key, node .. '|' .. ARGV[i], now) end
local out = {}
local rest = redis.call('HGETALL', key)
for i = 1, #rest, 2 do
  local f = rest[i]
  out[#out + 1] = string.sub(f, string.find(f, '|', 1, true) + 1)
end
if #rest > 0 then redis.call('PEXPIRE', key, window * 2) end
return out`

var setIPsScript = redis.NewScript(setIPsLua)

func (r *redisState) setNodeIPs(userID, nodeID string, ips []string, now time.Time) []string {
	ctx, cancel := r.ctx()
	defer cancel()
	args := []any{nodeID, now.UnixMilli(), onlineIPWindow.Milliseconds()}
	for _, ip := range ips {
		args = append(args, ip)
	}
	res, err := setIPsScript.Run(ctx, r.c, []string{r.prefix + "online:" + userID}, args...).StringSlice()
	if err != nil && err != redis.Nil {
		r.failed(err)
		return r.mem.setNodeIPs(userID, nodeID, ips, now)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(res))
	for _, ip := range res {
		if !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	return out
}

func (r *redisState) onlineIPs(userIDs []string) map[string]map[string]map[string]time.Time {
	if len(userIDs) == 0 {
		return map[string]map[string]map[string]time.Time{}
	}
	ctx, cancel := r.ctx()
	defer cancel()
	p := r.c.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(userIDs))
	for i, u := range userIDs {
		cmds[i] = p.HGetAll(ctx, r.prefix+"online:"+u)
	}
	if _, err := p.Exec(ctx); err != nil && err != redis.Nil {
		r.failed(err)
		return r.mem.onlineIPs(userIDs)
	}
	out := map[string]map[string]map[string]time.Time{}
	for i, u := range userIDs {
		for f, v := range cmds[i].Val() {
			node, ip, ok := strings.Cut(f, "|")
			ms, err := strconv.ParseInt(v, 10, 64)
			if !ok || err != nil {
				continue
			}
			if out[u] == nil {
				out[u] = map[string]map[string]time.Time{}
			}
			if out[u][node] == nil {
				out[u][node] = map[string]time.Time{}
			}
			out[u][node][ip] = time.UnixMilli(ms)
		}
	}
	return out
}

// allow uses a fixed one-minute window per key, shared by all instances;
// the in-memory limiter allows short bursts of 10 above the rate, so the
// window does too.
func (r *redisState) allow(name string, perMinute int, key string) bool {
	if perMinute <= 0 {
		perMinute = 30
	}
	ctx, cancel := r.ctx()
	defer cancel()
	k := fmt.Sprintf("%srl:%s:%s:%d", r.prefix, name, key, time.Now().Unix()/60)
	p := r.c.TxPipeline()
	incr := p.Incr(ctx, k)
	p.Expire(ctx, k, 2*time.Minute)
	if _, err := p.Exec(ctx); err != nil {
		r.failed(err)
		return r.mem.allow(name, perMinute, key)
	}
	return incr.Val() <= int64(max(perMinute, 10))
}

func (r *redisState) once(key string, every time.Duration) bool {
	ctx, cancel := r.ctx()
	defer cancel()
	ok, err := r.c.SetNX(ctx, r.prefix+"once:"+key, 1, every).Result()
	if err != nil {
		r.failed(err)
		return r.mem.once(key, every)
	}
	return ok
}
