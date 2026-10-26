package panel

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Renewal reminders for users: expiry notices come in tiers (by default 7, 3
// and 1 day before), each sent once per term, and every reminder about
// expiry or traffic carries a way to renew: shop buttons in Telegram, links
// in mail.

// expireTiers parses remind_expire_days: "7,3,1" (tiers), a single number N
// (N days, plus the last day) or "0" (off).
func expireTiers(raw string) []int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []int{7, 3, 1}
	}
	seen := map[int]bool{}
	var out []int
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '，' || r == ' ' }) {
		n, err := strconv.Atoi(f)
		if err != nil || n <= 0 || n > 60 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 1 && out[0] > 1 && !strings.Contains(raw, ",") {
		out = append(out, 1) // a plain "3" used to remind daily: keep the last day
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

// tierFor is the tightest tier containing left (the smallest t with
// left <= t days), or 0 when left is beyond every tier.
func tierFor(tiers []int, left time.Duration) int {
	tier := 0
	for _, t := range tiers {
		if left <= time.Duration(t)*24*time.Hour {
			tier = t
		}
	}
	return tier
}

// expireReminderDue reports whether u should hear about its expiry now: it
// entered a tighter tier than the one it was last told about. A renewal
// moves ExpireAt, so the old warning falls outside every tier again.
func expireReminderDue(tiers []int, u *store.User, now time.Time) bool {
	if u.ExpireAt == nil || !u.ExpireAt.After(now) || len(tiers) == 0 {
		return false
	}
	cur := tierFor(tiers, u.ExpireAt.Sub(now))
	if cur == 0 {
		return false
	}
	if u.WarnedExpire == nil {
		return true
	}
	last := tierFor(tiers, u.ExpireAt.Sub(*u.WarnedExpire))
	return last == 0 || cur < last
}

// renewKinds are the user notices that should offer a way to renew.
var renewKinds = map[string]bool{"expire_warn": true, "user_expired": true, "traffic_warn": true, "user_limited": true}

// userRenewButtons are the Telegram buttons under a renewal notice.
func (s *Server) userRenewButtons(kind string, u *store.User) [][]button {
	if !renewKinds[kind] {
		return nil
	}
	var rows [][]button
	if s.shopOn() {
		label := "🛒 续费套餐"
		if u.PlanID == "" {
			label = "🛒 购买套餐"
		}
		rows = append(rows, []button{{Text: label, Data: "v:shop"}})
		if (kind == "traffic_warn" || kind == "user_limited") && len(s.salePacks(u)) > 0 {
			rows = append(rows, []button{{Text: "📦 加购流量包", Data: "v:packs"}})
		}
	}
	if s.portalEnabled() {
		rows = append(rows, []button{{Text: "👤 用户中心", URL: s.portalURL()}})
	}
	return rows
}

// userRenewLines are the renewal links appended to a mail notice.
func (s *Server) userRenewLines(kind string) string {
	if !renewKinds[kind] {
		return ""
	}
	if s.webShopOn() {
		return "\n\n续费或加购流量：" + s.subBase() + shopWebPath
	}
	if s.shopOn() {
		return "\n\n可在 Telegram 机器人的「商店」里续费或加购流量。"
	}
	return ""
}
