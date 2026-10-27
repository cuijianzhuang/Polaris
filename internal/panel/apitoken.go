package panel

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// API tokens ("plt_…") let scripts and plugins call the API without a login
// session. A token reaches only the routes its scopes list below; anything
// else is refused, so a new endpoint is closed to tokens until it is added.

const apiTokenPrefix = "plt_"

// Scopes a token can hold.
const (
	ScopeUsersRead    = "users:read"
	ScopeUsersWrite   = "users:write"
	ScopeUsersRenew   = "users:renew"
	ScopeTrafficAdd   = "users:traffic:add"
	ScopeUsersPlan    = "users:plan"
	ScopeSubsRead     = "subs:read"
	ScopeNodesRead    = "nodes:read"
	ScopeTrafficRead  = "traffic:read"
	ScopeSettingsRead = "settings:read"
	// ScopeNodesMetrics writes custom metrics for nodes, nothing else.
	ScopeNodesMetrics = "nodes:metrics:write"
	// ScopeCDNIPs reports a speed-test point's best CDN IPs, nothing else.
	ScopeCDNIPs = "cdn:ips:write"
)

var allScopes = []string{ScopeUsersRead, ScopeUsersWrite, ScopeNodesRead, ScopeTrafficRead, ScopeSettingsRead, ScopeUsersRenew, ScopeTrafficAdd, ScopeSubsRead, ScopeUsersPlan, ScopeNodesMetrics, ScopeCDNIPs}

// tokenRoutes maps "METHOD /route" (gin's full path below the API base) to
// the scope it needs. "" means any valid token.
var tokenRoutes = map[string]string{
	"GET /tokens/self":   "",
	"GET /plugin/config": "", // a plugin's own token only; checked in the handler
	"GET /openapi.json":  "",

	"GET /users":                          ScopeUsersRead,
	"GET /users/export":                   ScopeUsersRead,
	"GET /users/:id":                      ScopeUsersRead,
	"GET /users/:id/traffic-sources":      ScopeTrafficRead,
	"GET /users/:id/links":                ScopeSubsRead,
	"GET /users/:id/sub-check":            ScopeSubsRead,
	"GET /users/:id/devices":              ScopeUsersRead,
	"GET /users/:id/node-quotas":          ScopeUsersRead,
	"GET /groups":                         ScopeUsersRead,
	"POST /users":                         ScopeUsersWrite,
	"POST /users/bulk":                    ScopeUsersWrite,
	"PUT /users/:id":                      ScopeUsersWrite,
	"DELETE /users/:id":                   ScopeUsersWrite,
	"PUT /users/:id/traffic":              ScopeUsersWrite,
	"POST /users/:id/reset-traffic":       ScopeUsersWrite,
	"POST /users/:id/reset-token":         ScopeUsersWrite,
	"POST /users/:id/renew":               ScopeUsersRenew,
	"POST /users/:id/add-traffic":         ScopeTrafficAdd,
	"POST /users/:id/plan":                ScopeUsersPlan,
	"GET /plans":                          ScopeUsersPlan,
	"POST /users/:id/pack":                ScopeUsersPlan,
	"GET /traffic-packs":                  ScopeUsersPlan,
	"DELETE /users/:id/devices/:did":      ScopeUsersWrite,
	"PUT /users/:id/node-quotas/:node":    ScopeUsersWrite,
	"DELETE /users/:id/node-quotas/:node": ScopeUsersWrite,

	"GET /nodes":                           ScopeNodesRead,
	"GET /nodes/:id/snell-readiness":       ScopeNodesRead,
	"GET /nodes/:id":                       ScopeNodesRead,
	"GET /nodes/:id/metrics/external":      ScopeNodesRead,
	"POST /nodes/:id/metrics":              ScopeNodesMetrics,
	"POST /cdn-profiles/:id/preferred-ips": ScopeCDNIPs,
	"GET /inbounds":                        ScopeNodesRead,

	"GET /stats/overview": ScopeTrafficRead,
	"GET /stats/traffic":  ScopeTrafficRead,
	"GET /stats/nodes":    ScopeTrafficRead,
	"GET /stats/tags":     ScopeTrafficRead,

	"GET /settings": ScopeSettingsRead,
}

// newTokenSecret returns "plt_" plus 40 base62 characters (~238 bits).
func newTokenSecret() string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 40)
	n := big.NewInt(int64(len(alphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, n)
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[v.Int64()]
	}
	return apiTokenPrefix + string(b)
}

// authToken authenticates an API token and enforces its scopes. It runs
// inside authRequired for bearer values starting with "plt_".
func (s *Server) authToken(c *gin.Context, tok string) {
	var t store.APIToken
	if s.st.DB.First(&t, "hash = ?", hashToken(tok)).Error != nil || (t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt)) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "API 令牌无效或已过期"})
		return
	}
	if t.PluginID != "" {
		var n int64
		s.st.DB.Model(&store.Plugin{}).Where("id = ? AND enabled = ?", t.PluginID, true).Count(&n)
		if n == 0 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "插件已停用或已卸载"})
			return
		}
	}
	route := c.Request.Method + " " + strings.TrimPrefix(c.FullPath(), s.cfg.APIBase())
	need, ok := tokenRoutes[route]
	if !ok {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "API 令牌不能访问此接口"})
		return
	}
	if !tokenAllows(&t, route) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "API 令牌缺少权限 " + need, "scope": need})
		return
	}
	now := time.Now()
	if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) > time.Minute {
		s.st.DB.Model(&store.APIToken{}).Where("id = ?", t.ID).Updates(map[string]any{"last_used_at": now, "last_ip": clientIP(c.Request)})
	}
	who := "token:" + t.Name
	if t.PluginID != "" {
		who = "plugin:" + t.PluginID
	}
	c.Set("admin", who)
	c.Set("apiToken", &t)
	c.Next()
}

func apiTokenOf(c *gin.Context) *store.APIToken {
	if v, ok := c.Get("apiToken"); ok {
		return v.(*store.APIToken)
	}
	return nil
}

// --- API ---

func (s *Server) listTokens(c *gin.Context) {
	var ts []store.APIToken
	s.st.DB.Order("created_at desc").Find(&ts)
	c.JSON(http.StatusOK, gin.H{"tokens": ts, "scopes": allScopes})
}

// createToken is POST /tokens {name, scopes, days}: the token is shown once.
func (s *Server) createToken(c *gin.Context) {
	var req struct {
		Name   string   `json:"name" binding:"required"`
		Scopes []string `json:"scopes"`
		Days   int      `json:"days"` // 0 = never expires
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	t, secret, err := s.issueAPIToken(strings.TrimSpace(req.Name), "", req.Scopes, req.Days, c.GetString("admin"))
	if err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "token.create", t.Name, strings.Join(t.Scopes.V, ","))
	c.JSON(http.StatusCreated, gin.H{"token": t, "secret": secret})
}

// issueAPIToken stores a new token and returns it with its secret.
func (s *Server) issueAPIToken(name, pluginID string, scopes []string, days int, by string) (*store.APIToken, string, error) {
	if name == "" || len(name) > 64 {
		return nil, "", errors.New("名称不能为空，最长 64 个字符")
	}
	clean := []string{}
	for _, sc := range scopes {
		if !slices.Contains(allScopes, sc) {
			return nil, "", fmt.Errorf("未知的权限范围 %q", sc)
		}
		if !slices.Contains(clean, sc) {
			clean = append(clean, sc)
		}
	}
	sort.Strings(clean)
	if days < 0 || days > 3650 {
		return nil, "", errors.New("有效期为 0（永久）到 3650 天")
	}
	secret := newTokenSecret()
	t := &store.APIToken{ID: store.NewID(), Name: name, PluginID: pluginID, Hash: hashToken(secret), Prefix: secret[:10],
		Scopes: store.JSON[[]string]{V: clean}, CreatedBy: by}
	if days > 0 {
		exp := time.Now().AddDate(0, 0, days)
		t.ExpiresAt = &exp
	}
	if err := s.st.DB.Create(t).Error; err != nil {
		return nil, "", err
	}
	return t, secret, nil
}

func (s *Server) deleteToken(c *gin.Context) {
	var t store.APIToken
	if err := s.st.DB.First(&t, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	s.st.DB.Delete(&t)
	s.auditCtx(c, "token.delete", t.Name, "")
	c.Status(http.StatusNoContent)
}

// tokenSelf is GET /tokens/self: what the calling token may do.
func (s *Server) tokenSelf(c *gin.Context) {
	t := apiTokenOf(c)
	if t == nil {
		c.JSON(http.StatusOK, gin.H{"admin": c.GetString("admin"), "scopes": allScopes})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": t.ID, "name": t.Name, "pluginId": t.PluginID, "scopes": t.Scopes.V, "expiresAt": t.ExpiresAt, "version": Version})
}

// tokenAllows is shared by authentication and the filtered OpenAPI view.
// Existing broad scopes remain compatible; new tokens can request less.
func tokenAllows(t *store.APIToken, route string) bool {
	need, ok := tokenRoutes[route]
	if !ok {
		return false
	}
	if need == "" || slices.Contains(t.Scopes.V, need) {
		return true
	}
	if (need == ScopeUsersRenew || need == ScopeTrafficAdd) && slices.Contains(t.Scopes.V, ScopeUsersWrite) {
		return true
	}
	return need == ScopeSubsRead && slices.Contains(t.Scopes.V, ScopeUsersRead)
}
