package panel

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// A revoked session must close the live stream, not only the REST API.
func TestSessionCheckRevoked(t *testing.T) {
	s, _, _ := newAdminClient(t)
	var a store.Admin
	s.st.DB.First(&a, "username = ?", "root")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/ws", nil)
	sid := s.newSession(c, a.ID, a.Username, "password", time.Hour)
	c.Set("sid", sid)
	c.Set("adminID", a.ID)
	c.Set("tokenVersion", a.TokenVersion)
	check := s.sessionCheck(c)
	if !check() {
		t.Fatal("live session rejected")
	}
	s.st.DB.Model(&store.AdminSession{}).Where("id = ?", sid).Update("revoked", true)
	if check() {
		t.Fatal("revoked session still accepted by the WebSocket check")
	}
}
