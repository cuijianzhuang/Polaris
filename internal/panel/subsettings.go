package panel

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Subscription presentation settings (3x-ui style): a separate public base
// URL, profile title, support link, announcement, usage info in node names and
// a client routing profile header.

func (s *Server) subEnabled() bool { return s.st.GetSetting("sub_enabled", "true") != "false" }

// subBase is the public origin for subscription links.
func (s *Server) subBase() string {
	if b := strings.TrimRight(s.st.GetSetting("sub_base_url", ""), "/"); b != "" {
		return b
	}
	return s.panelBase()
}

// panelBase is the public origin of the panel itself: links into the admin
// UI must not use the subscription domain, which serves only subscriptions.
func (s *Server) panelBase() string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	// Without a configured URL, use the address the admin reaches the
	// panel at: "http://" + Listen is ":8080" by default, which clients
	// cannot fetch.
	if b, _ := s.seenBase.Load().(string); b != "" {
		return b
	}
	if b := s.st.GetSetting("seen_base_url", ""); b != "" {
		s.seenBase.Store(b)
		return b
	}
	return "http://" + listenHost(s.cfg.Listen)
}

// noteBase remembers the public address of an admin request (not loopback:
// that is an SSH tunnel or the server itself) for subscription links.
func (s *Server) noteBase(r *http.Request) {
	if s.cfg.PublicURL != "" || r == nil || !validHost.MatchString(r.Host) {
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); host == "localhost" || ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return
	}
	b := s.requestBase(r)
	if old, _ := s.seenBase.Load().(string); old == b {
		return
	}
	s.seenBase.Store(b)
	_ = s.st.SetSetting("seen_base_url", b)
}

// listenHost turns a listen address such as ":8080" or "0.0.0.0:8080" into
// something dialable (the best effort when nothing else is known).
func listenHost(listen string) string {
	h, p, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(h); h == "" || ip != nil && ip.IsUnspecified() {
		h = "127.0.0.1"
	}
	return net.JoinHostPort(h, p)
}

func (s *Server) subTitle(fallback string) string {
	return orDefault(s.st.GetSetting("sub_title", ""), orDefault(fallback, s.st.GetSetting("site_title", "Polaris")))
}

// decorateNodes applies the "show usage info" setting to a node list. In
// "auto" mode only clients that cannot show the profile headers get info
// nodes (see sub.ReadsProfileHeaders); headersShown says the client is
// known to read them.
func (s *Server) decorateNodes(nodes []sub.Node, info sub.Info, headersShown bool) []sub.Node {
	mode := s.st.GetSetting("sub_show_info", "none")
	if mode == "auto" {
		mode = "none"
		if !headersShown {
			mode = "nodes"
		}
	}
	switch mode {
	case "nodes":
		return append(sub.InfoNodes(info), nodes...)
	case "suffix":
		if suf := sub.InfoSuffix(info); suf != "" {
			out := make([]sub.Node, len(nodes))
			copy(out, nodes)
			for i := range out {
				out[i].Name += suf
			}
			return out
		}
	}
	return nodes
}

// subHeaders writes the client-facing profile headers.
func (s *Server) subHeaders(c *gin.Context, info sub.Info) {
	c.Header("subscription-userinfo", sub.UserInfoHeader(info))
	c.Header("profile-update-interval", s.st.GetSetting("sub_update_interval", "1"))
	c.Header("profile-title", "base64:"+b64(info.Title))
	c.Header("profile-web-page-url", info.URL)
	if v := s.st.GetSetting("sub_support_url", ""); v != "" {
		c.Header("support-url", v)
	}
	if v := strings.TrimSpace(s.st.GetSetting("sub_announce", "")); v != "" {
		c.Header("announce", "base64:"+b64(v))
	}
	if s.st.GetSetting("sub_routing_enabled", "false") == "true" {
		if v := routingHeader(s.st.GetSetting("sub_routing_rules", "")); v != "" {
			c.Header("routing", v)
		}
	}
}

// routingHeader accepts a ready happ:// link or a routing profile JSON.
func routingHeader(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "happ://") {
		return v
	}
	return "happ://routing/onadd/" + base64.StdEncoding.EncodeToString([]byte(v))
}

// pageExtras fills the info-page-only fields.
func (s *Server) pageExtras(info *sub.Info, u *store.User, nodes []sub.Node) {
	info.Announce = strings.TrimSpace(s.st.GetSetting("sub_announce", ""))
	info.SupportURL = s.st.GetSetting("sub_support_url", "")
	for _, n := range nodes {
		if n.Source == "info" {
			continue
		}
		info.Links = append(info.Links, sub.PageLink{Name: n.Name, Protocol: sub.ProtocolLabel(n), Link: sub.ShareLink(n)})
	}
	if u == nil {
		return
	}
	info.Status = u.Status
	if u.PlanID != "" {
		var p store.Group
		if s.st.DB.First(&p, "id = ? AND plan = ?", u.PlanID, true).Error == nil {
			info.PlanName = p.Name
			var next store.Group
			if p.FallbackID != "" && s.st.DB.First(&next, "id = ? AND plan = ?", p.FallbackID, true).Error == nil {
				info.PlanNext = next.Name
			}
		}
	}
	planInfo(info, u)
	info.SpeedNote = s.speedNote(u)
	if s.tg != nil && s.tg.isRunning() {
		s.tg.mu.Lock()
		me := s.tg.me
		s.tg.mu.Unlock()
		if me != "" && s.st.GetSetting("sub_show_bot", "true") == "true" {
			info.BotURL = "https://t.me/" + me + "?start=" + u.SubToken
		}
	}
	info.Daily = s.dailyUsage(u.ID, 30)
	info.NodeQuotas = s.nodeQuotaLines(u.ID)
}

// dailyUsage returns the last n days of a user's traffic, oldest first.
func (s *Server) dailyUsage(userID string, n int) []sub.DayUsage {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(n - 1))
	var rows []store.TrafficHourly
	s.st.DB.Where("user_id = ? AND hour >= ?", userID, start.UTC()).Find(&rows)
	out := make([]sub.DayUsage, n)
	for i := range out {
		out[i].Day = start.AddDate(0, 0, i)
	}
	for _, r := range rows {
		l := r.Hour.Local()
		d := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, l.Location())
		i := int(d.Sub(start).Hours()/24 + 0.5)
		if i >= 0 && i < n {
			out[i].Up += r.Up
			out[i].Down += r.Down
		}
	}
	return out
}

// tgBroadcast sends an announcement to every bound user (and optionally admins).
func (s *Server) tgBroadcast(c *gin.Context) {
	var req struct {
		Text   string `json:"text" binding:"required"`
		Admins bool   `json:"admins"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if !s.tg.isRunning() {
		badRequest(c, fmt.Errorf("机器人未运行：请检查 Token 与网络"))
		return
	}
	var chats []int64
	s.st.DB.Model(&store.User{}).Where("telegram_id <> 0 AND is_service = ?", false).Distinct().Pluck("telegram_id", &chats)
	if req.Admins {
		var as []int64
		s.st.DB.Model(&store.TGAdmin{}).Pluck("chat_id", &as)
		chats = append(chats, as...)
	}
	seen := map[int64]bool{}
	uniq := chats[:0]
	for _, id := range chats {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	text := "📢 <b>公告</b>\n" + esc(req.Text)
	go func() {
		for _, id := range uniq {
			s.tg.send(id, text)
			time.Sleep(50 * time.Millisecond) // stay under Telegram's 30 msg/s limit
		}
	}()
	s.auditCtx(c, "tg.broadcast", fmt.Sprintf("%d chats", len(uniq)), req.Text)
	c.JSON(http.StatusOK, gin.H{"queued": len(uniq)})
}

// planInfo fills the plan details shown in info nodes and on the info page.
func planInfo(info *sub.Info, u *store.User) {
	if u == nil {
		return
	}
	info.ResetNote = ""
	switch u.ResetPolicy {
	case "daily":
		info.ResetNote = "每日重置流量"
	case "weekly":
		info.ResetNote = fmt.Sprintf("每周%s重置流量", []string{"一", "一", "二", "三", "四", "五", "六", "日"}[min(max(u.ResetDay, 1), 7)])
	case "monthly":
		info.ResetNote = fmt.Sprintf("每月 %d 日重置流量", max(1, u.ResetDay))
	case "cycle":
		info.ResetNote = fmt.Sprintf("每 %d 天重置流量", max(1, u.ResetDay))
	}
	if u.ExpireAt == nil {
		info.ExpireAfterDays = u.ExpireAfterDays
	}
}

// speedNote describes the user's speed limit for the info page, or "" when
// none applies (no limit, or only Hysteria2 inbounds, where it is not enforced).
func (s *Server) speedNote(u *store.User) string {
	up, down := effectiveSpeed(u, s.groupPlans([]string{u.ID})[u.ID])
	if up <= 0 && down <= 0 {
		return ""
	}
	var n int64
	s.st.DB.Raw("SELECT COUNT(*) FROM ("+store.GrantsSQL+") g JOIN inbounds i ON i.id = g.inbound_id WHERE g.user_id = ? AND i.core <> ?",
		u.ID, string(model.CoreHysteria)).Scan(&n)
	if n == 0 {
		return ""
	}
	f := func(v int) string {
		if v <= 0 {
			return "不限"
		}
		return fmt.Sprintf("%d Mbps", v)
	}
	return "↑ " + f(up) + " · ↓ " + f(down)
}
