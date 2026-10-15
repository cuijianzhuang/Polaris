package panel

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Subscription diagnosis: why a user's subscription has (or lacks) nodes.

// emptyReason explains an empty subscription in one line.
func (s *Server) emptyReason(u *store.User) string {
	switch u.Status {
	case "expired":
		return "账号已到期，请联系管理员续期"
	case "limited":
		return "流量已用完，请联系管理员"
	case "disabled":
		return "账号已停用"
	}
	if u.BannedUntil != nil && u.BannedUntil.After(time.Now()) {
		return "设备数超限，暂时封禁中"
	}
	var n int64
	s.st.DB.Raw("SELECT COUNT(*) FROM ("+store.GrantsSQL+") g WHERE user_id = ?", u.ID).Scan(&n)
	if n == 0 {
		return "账号未授权任何节点，请联系管理员"
	}
	return "授权的节点暂不可用，请联系管理员"
}

// reasonNode is a placeholder entry carrying the reason, so clients show a
// message instead of an unexplained empty list.
func reasonNode(reason string) sub.Node {
	return sub.Node{Name: "⚠️ " + reason, Protocol: "shadowsocks", Server: "127.0.0.1", Port: 1,
		Method: "aes-128-gcm", Password: "info", Source: "info"}
}

type diagInbound struct {
	ID       string   `json:"id"`
	Tag      string   `json:"tag"`
	Node     string   `json:"node"`
	Via      []string `json:"via"`      // node / inbound / group:<name>
	Included bool     `json:"included"` // appears in the subscription
	Reason   string   `json:"reason,omitempty"`
}

// subDiagnose is GET /users/:id/sub-check.
func (s *Server) subDiagnose(c *gin.Context) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	type grant struct{ InboundID string }
	var gs []grant
	s.st.DB.Raw("SELECT DISTINCT inbound_id FROM ("+store.GrantsSQL+") g WHERE user_id = ?", u.ID).Scan(&gs)
	ids := make([]string, 0, len(gs))
	for _, g := range gs {
		ids = append(ids, g.InboundID)
	}
	via := map[string][]string{}
	var acc []store.UserAccess
	s.st.DB.Where("user_id = ?", u.ID).Find(&acc)
	nodeBound := map[string]bool{}
	for _, a := range acc {
		if a.Kind == "node" {
			nodeBound[a.TargetID] = true
		} else {
			via[a.TargetID] = append(via[a.TargetID], "inbound")
		}
	}
	type gv struct{ InboundID, Name string }
	var gvs []gv
	s.st.DB.Table("user_groups").Select(`group_inbounds.inbound_id AS inbound_id, "groups".name AS name`).
		Joins("JOIN group_inbounds ON group_inbounds.group_id = user_groups.group_id").
		Joins(`JOIN "groups" ON "groups".id = user_groups.group_id`).
		Where("user_groups.user_id = ?", u.ID).Scan(&gvs)
	for _, g := range gvs {
		via[g.InboundID] = append(via[g.InboundID], "group:"+g.Name)
	}
	var ins []store.Inbound
	if len(ids) > 0 {
		s.st.DB.Where("id IN ?", ids).Order("node_id, port").Find(&ins)
	}
	nodes := map[string]store.Node{}
	var ns []store.Node
	s.st.DB.Find(&ns)
	for _, n := range ns {
		nodes[n.ID] = n
	}
	out := make([]diagInbound, 0, len(ins))
	for _, in := range ins {
		d := diagInbound{ID: in.ID, Tag: in.Tag, Via: via[in.ID], Included: true}
		if nodeBound[in.NodeID] {
			d.Via = append([]string{"node"}, d.Via...)
		}
		n, ok := nodes[in.NodeID]
		d.Node = n.Name
		switch {
		case !ok:
			d.Included, d.Reason = false, "节点已删除"
		case n.Revoked:
			d.Included, d.Reason = false, "节点证书已吊销"
		case !in.Enabled:
			d.Included, d.Reason = false, "入站已停用"
		case in.ServiceOnly:
			d.Included, d.Reason = false, "仅供链路内部使用"
		case nodeHost(&n) == "":
			d.Reason = "节点没有公网地址（未填写，Agent 也未检测到），链接里的服务器地址为空"
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Included && !out[j].Included })

	// What each client format would actually contain.
	formats := map[string]int{}
	var problems []string
	if u.Status != "active" {
		problems = append(problems, s.emptyReason(&u))
	}
	if subNodes, err := s.subNodes(&u); err == nil {
		formats["links"] = len(subNodes)
		for _, n := range subNodes {
			if sub.ClashProxy(n) != nil {
				formats["clash"]++
			}
			if sub.SingboxOutbound(n) != nil {
				formats["singbox"]++
			}
			if sub.SurgeLine(n) != "" {
				formats["surge"]++
			}
			if sub.LoonLine(n) != "" {
				formats["loon"]++
			}
			if sub.QuanxLine(n) != "" {
				formats["quanx"]++
			}
		}
	}
	if len(ids) == 0 {
		problems = append(problems, "未授权任何入站：在「授权」中绑定节点、选择分组或单独授权入站")
	} else if formats["links"] == 0 {
		problems = append(problems, "已授权的入站都不可用（见下表原因）")
	}
	if !s.subEnabled() {
		problems = append(problems, "订阅功能已在「订阅设置」中关闭")
	}
	c.JSON(http.StatusOK, gin.H{"status": u.Status, "problems": nonNilStrs(problems), "inbounds": out, "formats": formats,
		"url": s.subURL(u.SubToken)})
}

var subLinkRe = regexp.MustCompile(`/sub/(p/)?([A-Za-z0-9_-]{16,})`)
var subTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)

// userFromSubLink resolves a pasted subscription link (or bare token) to its
// account: user links, subscription-group links (through the group's panel
// user), extra text around the link, query strings and trailing slashes are
// all accepted. On failure it returns a message saying why.
func (s *Server) userFromSubLink(text string) (*store.User, string) {
	text = strings.TrimSpace(text)
	profile, tok := false, ""
	if m := subLinkRe.FindStringSubmatch(text); m != nil {
		profile, tok = m[1] != "", m[2]
	} else if f := strings.Fields(text); len(f) > 0 && subTokenRe.MatchString(f[0]) {
		tok = f[0]
	}
	if tok == "" {
		return nil, "没有识别到订阅链接：请发送 /bind 加上完整的订阅链接（形如 https://…/sub/xxxx）"
	}
	var u store.User
	if profile {
		var p store.SubProfile
		if s.st.DB.First(&p, "token = ?", tok).Error != nil {
			return nil, "未找到该订阅组链接，可能已被重置，请向管理员索取新链接"
		}
		if p.PanelUserID == "" || s.st.DB.First(&u, "id = ? AND is_service = ?", p.PanelUserID, false).Error != nil {
			return nil, "这是订阅组链接，未关联面板用户，无法绑定账号；请使用个人订阅链接"
		}
		return &u, ""
	}
	if s.st.DB.First(&u, "sub_token = ? AND is_service = ?", tok, false).Error != nil {
		return nil, "未找到该订阅对应的账号（链接标识 " + tok[:4] + "…" + tok[len(tok)-4:] + "）。可能原因：链接已被重置（旧链接失效），请获取最新链接；或同一个机器人还被另一个面板使用（如重装前的旧面板），请管理员确认。"
	}
	return &u, ""
}
