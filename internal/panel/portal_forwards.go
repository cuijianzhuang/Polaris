package panel

import (
	_ "embed"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// "My forwards" in the user center: list, add and delete self-service port
// forwards (see forward_shop.go).

type portalFwdRow struct {
	ID, Name, Node, Entry, Target, Network string
}

type portalFwdNode struct{ ID, Name string }

// portalForwards is GET /u/forwards.
func (s *Server) portalForwards(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalUser(c)
	if u == nil {
		c.Redirect(http.StatusFound, portalPath)
		return
	}
	var rs []store.ForwardRule
	s.st.DB.Where("user_id = ?", u.ID).Order("created_at, id").Find(&rs)
	nodes := map[string]*store.Node{}
	var ns []store.Node
	s.st.DB.Find(&ns)
	for i := range ns {
		nodes[ns[i].ID] = &ns[i]
	}
	rows := make([]portalFwdRow, 0, len(rs))
	for _, r := range rs {
		row := portalFwdRow{ID: r.ID, Name: r.Name, Network: r.Network}
		if n := nodes[r.NodeID]; n != nil {
			row.Node, row.Entry = n.Name, net.JoinHostPort(nodeHost(n), strconv.Itoa(r.ListenPort))
		}
		if len(r.Remotes.V) > 0 {
			row.Target = r.Remotes.V[0].Addr
		}
		rows = append(rows, row)
	}
	var opts []portalFwdNode
	for _, n := range s.fwdNodes(u.ID) {
		opts = append(opts, portalFwdNode{ID: n.ID, Name: n.Name})
	}
	lo, hi := fwdPorts(u.ForwardEntitlement)
	used, quota := bytesText(u.FwdUsed), "不限"
	if u.FwdTraffic > 0 {
		quota = bytesText(u.FwdTraffic)
	}
	state := ""
	switch {
	case u.FwdMax == 0:
		state = "你的套餐不包含端口转发。"
	case !s.fwdServes(u) && u.Status != "active":
		state = "账号未处于正常状态，转发已暂停。"
	case !s.fwdServes(u):
		state = "本周期的转发流量已用完，转发已暂停，下个周期自动恢复。"
	}
	data := map[string]any{"Title": s.subTitle(s.st.GetSetting("site_title", "Polaris")), "Rows": rows, "Nodes": opts,
		"Max": u.FwdMax, "Count": len(rs), "Lo": lo, "Hi": hi, "Used": used, "Quota": quota, "State": state,
		"Speed": u.FwdSpeedMbps, "CanAdd": u.FwdMax > len(rs) && len(opts) > 0 && s.fwdServes(u)}
	s.renderPortalSection(c, http.StatusOK, "account", "我的转发", portalFwdTpl, data, u)
}

type bytesWriter struct{ b *[]byte }

func (w *bytesWriter) Write(p []byte) (int, error) { *w.b = append(*w.b, p...); return len(p), nil }

func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n>>10)
	}
}

func (s *Server) portalFwdWriter(c *gin.Context) *store.User {
	if !portalWrite(c) {
		return nil
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return nil
	}
	if !s.ticketLimit.Allow(clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "操作太频繁，请稍后再试"})
		return nil
	}
	return u
}

// portalForwardCreate is POST /u/forwards.
func (s *Server) portalForwardCreate(c *gin.Context) {
	u := s.portalFwdWriter(c)
	if u == nil {
		return
	}
	var req userForwardReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	row, err := s.createUserForward(c.Request.Context(), u, req)
	if err != nil {
		badRequest(c, err)
		return
	}
	s.audit("user:"+u.Username, "forward.user_create", row.Name, fmt.Sprintf(":%d → %s", row.ListenPort, row.Remotes.V[0].Addr), clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": row.ID, "listenPort": row.ListenPort})
}

// portalForwardDelete is POST /u/forwards/:id/delete.
func (s *Server) portalForwardDelete(c *gin.Context) {
	u := s.portalFwdWriter(c)
	if u == nil {
		return
	}
	if err := s.deleteUserForward(u, c.Param("id")); err != nil {
		badRequest(c, err)
		return
	}
	s.audit("user:"+u.Username, "forward.user_delete", c.Param("id"), "", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

//go:embed assets/portal-forwards.html
var portalForwardsHTML string

var portalFwdTpl = template.Must(template.New("forwards").Parse(portalForwardsHTML))
