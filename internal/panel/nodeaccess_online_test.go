package panel

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

func TestNodeUserOnlineSourcesUseSharedFreshnessWindow(t *testing.T) {
	s := newTestServer(t)
	for _, row := range []any{
		&store.Node{ID: "n", Name: "N"},
		&store.User{ID: "u", Username: "U"},
		&store.UserAccess{UserID: "u", Kind: "node", TargetID: "n"},
	} {
		if err := s.st.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	memOnly(s).ips["u"] = map[string]map[string]time.Time{"n": {
		"198.51.100.1:443": now,
		"198.51.100.2":     now.Add(-onlineIPWindow - time.Second),
		"not-an-ip":        now,
	}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "id", Value: "n"}}
	s.nodeUsersAPI(c)
	var rows []nodeUserRow
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || len(rows) != 1 || len(rows[0].OnlineIP) != 1 || rows[0].OnlineIP[0] != "198.51.100.1" {
		t.Fatalf("node view kept stale or malformed sources: %s", recorder.Body.String())
	}
}
