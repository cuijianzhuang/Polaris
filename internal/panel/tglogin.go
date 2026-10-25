package panel

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Telegram sign-in: a Telegram admin sends /login to the bot and gets a
// one-time link. The owner's session has full access; an operator's session
// is limited to the nodes assigned to them (see scopeGuard).

const (
	tgLoginTTL   = 5 * time.Minute
	tgSessionTTL = 12 * time.Hour
)

// tgLoginCode is a one-time /login code, kept in the shared store so the
// link works on any panel instance.
type tgLoginCode struct {
	AdminID string `json:"a"`
}

func (s *Server) newTGLoginCode(adminID string) string {
	code := pki.Token(32)
	if err := s.eph.put("tglogin:"+code, tgLoginCode{AdminID: adminID}, tgLoginTTL); err != nil {
		s.log.Error("store telegram login code", "err", err)
	}
	return code
}

func (s *Server) takeTGLoginCode(code string) (string, bool) {
	var v tgLoginCode
	if code == "" || !s.eph.take("tglogin:"+code, &v) { // single use
		return "", false
	}
	return v.AdminID, true
}

// tgLoginURL is the link the bot sends; the code rides in the fragment so it
// never reaches server or proxy logs.
func (s *Server) tgLoginURL(code string) string {
	return s.panelBase() + strings.TrimRight(s.cfg.AdminPath, "/") + "/#tg=" + code
}

func (s *Server) issueTGToken(a *store.TGAdmin, gc *gin.Context) (string, error) {
	c := claims{
		SID: s.newSession(gc, "tg:"+a.ID, "tg:"+a.Name, "telegram", tgSessionTTL),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "tg:" + a.ID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(tgSessionTTL)),
		},
		Username: "tg:" + a.Name,
		TG:       a.ID,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.jwtKey)
}

// tgLogin exchanges a one-time code: POST /auth/tg {code}.
func (s *Server) tgLogin(c *gin.Context) {
	var req struct {
		Code string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	id, ok := s.takeTGLoginCode(req.Code)
	var a store.TGAdmin
	if !ok || s.st.DB.First(&a, "id = ?", id).Error != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "登录链接无效或已过期，请在机器人中重新发送 /login"})
		return
	}
	tok, err := s.issueTGToken(&a, c)
	if err != nil {
		serverError(c, err)
		return
	}
	s.audit("tg:"+a.Name, "admin.login", "tg:"+a.Name, "telegram ("+a.Role+")", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"token": tok, "username": "tg:" + a.Name, "expiresIn": int(tgSessionTTL.Seconds())})
}

// nodeScope is the set of nodes a session may see; nil means all nodes.
func nodeScope(c *gin.Context) map[string]bool {
	v, ok := c.Get("nodeScope")
	if !ok {
		return nil
	}
	return v.(map[string]bool)
}

func inScope(c *gin.Context, nodeID string) bool {
	sc := nodeScope(c)
	return sc == nil || sc[nodeID]
}

// scopedRoutes are the endpoints a node operator may call: viewing their
// nodes plus the actions the bot already offers them (sync, rollback, core
// restart). Everything else is refused.
var scopedRoutes = map[string]bool{
	"GET /auth/me":                      true,
	"POST /auth/logout":                 true,
	"POST /auth/sudo":                   true,
	"GET /auth/sessions":                true,
	"DELETE /auth/sessions/:id":         true,
	"POST /auth/sessions/revoke-others": true,
	"GET /ws":                           true,
	"GET /nodes":                        true,
	"GET /nodes/:id":                    true,
	"GET /nodes/:id/revisions":          true,
	"GET /nodes/:id/snell-readiness":    true,
	"GET /nodes/:id/cores":              true,
	"GET /nodes/:id/metrics":            true,
	"GET /nodes/:id/net-daily":          true,
	"POST /nodes/:id/sync":              true,
	"POST /nodes/:id/rollback":          true,
	"POST /cores/:id/restart":           true,
	"GET /cores/:id/logs":               true,
	"GET /capabilities":                 true,
	"GET /core-latest":                  true,
	"GET /stats/nodes":                  true,
	"GET /stats/traffic":                true, // only with ?node= in scope, see scopeGuard
}

// scopeGuard enforces scopedRoutes and the node scope of path parameters.
func (s *Server) scopeGuard(c *gin.Context) {
	sc := nodeScope(c)
	if sc == nil {
		c.Next()
		return
	}
	route := strings.TrimPrefix(c.FullPath(), s.cfg.APIBase())
	if !scopedRoutes[c.Request.Method+" "+route] {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "节点管理员无权访问此功能"})
		return
	}
	nodeID := ""
	switch {
	case strings.HasPrefix(route, "/nodes/:id"):
		nodeID = c.Param("id")
	case strings.HasPrefix(route, "/cores/:id"):
		var cr store.Core
		if s.st.DB.Select("node_id").First(&cr, "id = ?", c.Param("id")).Error != nil {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		nodeID = cr.NodeID
	case route == "/capabilities":
		nodeID = c.Query("node")
	case route == "/stats/traffic":
		if nodeID = c.Query("node"); nodeID == "" || c.Query("user") != "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "节点管理员只能查看自己节点的流量"})
			return
		}
	}
	if nodeID != "" && !sc[nodeID] {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "无权访问该节点"})
		return
	}
	c.Next()
}

// scopedNodeViews filters node views to a scope (nil = all).
func scopedNodeViews(vs []nodeView, sc map[string]bool) []nodeView {
	if sc == nil {
		return vs
	}
	out := make([]nodeView, 0, len(sc))
	for _, v := range vs {
		if sc[v.ID] {
			out = append(out, v)
		}
	}
	return out
}
