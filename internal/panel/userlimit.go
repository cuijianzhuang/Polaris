package panel

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

type limitedRow struct {
	UserID    string `json:"userId"`
	Username  string `json:"username"`
	UpKbps    int32  `json:"upKbps"`
	DownKbps  int32  `json:"downKbps"`
	UpBps     int64  `json:"upBps"`
	DownBps   int64  `json:"downBps"`
	Saturated bool   `json:"saturated"` // a direction at 90% of its allowance or more
}

// nodeLimited is GET /nodes/:id/limited: the users under a speed limit on
// the node and their current speed, busiest first (from the last heartbeat).
func (s *Server) nodeLimited(c *gin.Context) {
	s.hbMu.Lock()
	ls, ok := s.limited[c.Param("id")]
	s.hbMu.Unlock()
	if !ok {
		s.remoteHeartbeatState(c.Param("id"), "limited", &ls) // the node is connected to another instance
	}
	rows := []limitedRow{}
	ids := make([]string, 0, len(ls))
	for _, l := range ls {
		ids = append(ids, l.UserId)
	}
	names := map[string]string{}
	if len(ids) > 0 {
		var us []store.User
		s.st.DB.Select("id", "username").Where("id IN ?", ids).Find(&us)
		for _, u := range us {
			names[u.ID] = u.Username
		}
	}
	near := func(bps int64, kbps int32) bool { return kbps > 0 && bps*10 >= int64(kbps)*1000*9 }
	for _, l := range ls {
		rows = append(rows, limitedRow{UserID: l.UserId, Username: names[l.UserId], UpKbps: l.UpKbps, DownKbps: l.DownKbps,
			UpBps: l.UpBps, DownBps: l.DownBps, Saturated: near(l.UpBps, l.UpKbps) || near(l.DownBps, l.DownKbps)})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].UpBps+rows[i].DownBps > rows[j].UpBps+rows[j].DownBps
	})
	c.JSON(http.StatusOK, rows)
}
