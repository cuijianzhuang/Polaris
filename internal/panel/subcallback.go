package panel

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/pki"
)

// External subscription converters fetch our node list over HTTP. They get a
// short-lived, few-use callback URL instead of the subscriber's permanent
// token, so a third-party backend cannot keep pulling the subscription.

const (
	callbackTTL  = 3 * time.Minute
	callbackUses = 3
)

type subCallback struct {
	path    string // "/sub/<token>" or "/sub/p/<token>"
	expires time.Time
	uses    int
}

type callbackStore struct {
	mu sync.Mutex
	m  map[string]*subCallback
}

func (cs *callbackStore) issue(path string) string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.m == nil {
		cs.m = map[string]*subCallback{}
	}
	now := time.Now()
	for k, v := range cs.m { // lazy cleanup
		if now.After(v.expires) {
			delete(cs.m, k)
		}
	}
	id := pki.Token(24)
	cs.m[id] = &subCallback{path: path, expires: now.Add(callbackTTL)}
	return id
}

func (cs *callbackStore) take(id string) (string, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	v, ok := cs.m[id]
	if !ok || time.Now().After(v.expires) {
		delete(cs.m, id)
		return "", false
	}
	if v.uses++; v.uses >= callbackUses {
		delete(cs.m, id)
	}
	return v.path, true
}

// callbackURL is what the external converter receives.
func (s *Server) callbackURL(path string) string {
	return s.subBase() + "/sub/cb/" + s.callbacks.issue(path)
}

// subCallbackHandler serves /sub/cb/:id as the raw link list of its target.
func (s *Server) subCallbackHandler(c *gin.Context) {
	path, ok := s.callbacks.take(c.Param("token"))
	if !ok {
		c.String(http.StatusNotFound, "404 page not found")
		return
	}
	c.Request.URL.RawQuery = "raw=1"
	if tok, ok := strings.CutPrefix(path, "/sub/p/"); ok {
		c.Params = gin.Params{{Key: "token", Value: tok}}
		s.profileSubscription(c)
		return
	}
	c.Params = gin.Params{{Key: "token", Value: strings.TrimPrefix(path, "/sub/")}}
	s.subscription(c)
}
