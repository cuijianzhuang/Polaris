package panel

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Request body ceilings. Handlers that accept larger uploads (backup restore,
// route databases) enforce their own limits and are listed in
// bodyLimitExempt; handlers may always apply a tighter MaxBytesReader.
const (
	publicBodyLimit = 1 << 20  // subscription, portal, payment callbacks, login
	adminBodyLimit  = 16 << 20 // authenticated admin / API-token requests
)

// bodyLimitExempt lists route patterns (relative to the admin API base) that
// stream large uploads and bound them themselves.
var bodyLimitExempt = []string{
	"/backup/restore",
	"/backup/files/:name/restore",
	"/tools/route-geo/database",
	"/tools/route-asn/database",
}

// bodyLimit caps every request body so an unauthenticated client cannot make
// the panel buffer an arbitrarily large JSON or form body. It runs after
// routing, so the matched pattern decides the ceiling.
func (s *Server) bodyLimit(c *gin.Context) {
	if c.Request.Body == nil || c.Request.Body == http.NoBody {
		c.Next()
		return
	}
	limit := int64(publicBodyLimit)
	// Login and other public API calls carry no credentials; admin sessions
	// and API tokens always send a bearer token. A forged header only raises
	// the ceiling to adminBodyLimit; the auth middleware still rejects it.
	if api := s.cfg.APIBase(); strings.HasPrefix(c.Request.URL.Path, api+"/") && c.GetHeader("Authorization") != "" {
		route := strings.TrimPrefix(c.FullPath(), api)
		for _, p := range bodyLimitExempt {
			if route == p {
				c.Next()
				return
			}
		}
		limit = adminBodyLimit
	}
	if c.Request.ContentLength > limit {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "请求内容过大"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	c.Next()
}
