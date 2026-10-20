package panel

import (
	"net/http"
	"strings"
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
	Path string `json:"p"` // "/sub/<token>" or "/sub/p/<token>"
	Uses int    `json:"u"`
}

// callbackStore keeps callback links in the shared store: the converter may
// reach any panel instance.
type callbackStore struct{ eph *ephStore }

func (cs *callbackStore) issue(path string) string {
	id := pki.Token(24)
	_ = cs.eph.put("subcb:"+id, subCallback{Path: path}, callbackTTL)
	return id
}

func (cs *callbackStore) take(id string) (string, bool) {
	var v subCallback
	if id == "" {
		return "", false
	}
	ok := cs.eph.update("subcb:"+id, &v, func() ephAction {
		if v.Uses++; v.Uses >= callbackUses {
			return ephDelete
		}
		return ephSave
	})
	return v.Path, ok
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
