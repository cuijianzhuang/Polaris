package panel

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type userTrafficSource struct {
	Hour         time.Time `json:"hour"`
	NodeID       string    `json:"nodeId"`
	NodeName     string    `json:"nodeName"`
	Up           int64     `json:"up"`
	Down         int64     `json:"down"`
	RawUp        *int64    `json:"rawUp"`
	RawDown      *int64    `json:"rawDown"`
	AccountUp    *int64    `json:"accountUp"`
	AccountDown  *int64    `json:"accountDown"`
	RecordedUp   int64     `json:"recordedUp"`
	RecordedDown int64     `json:"recordedDown"`
	RateMin      *float64  `json:"rateMin"`
	RateMax      *float64  `json:"rateMax"`
	Coverage     string    `json:"coverage"`
}

func trafficSourceView(row store.TrafficHourly, name string) userTrafficSource {
	view := userTrafficSource{Hour: row.Hour, NodeID: row.NodeID, NodeName: name, Up: row.Up, Down: row.Down, RecordedUp: row.RecordedUp, RecordedDown: row.RecordedDown, Coverage: "legacy"}
	if row.RawRecorded {
		view.RawUp, view.RawDown = &row.RawUp, &row.RawDown
		view.AccountUp, view.AccountDown = &row.AccountUp, &row.AccountDown
		view.RateMin, view.RateMax = &row.RateMin, &row.RateMax
		view.Coverage = "complete"
		if row.RecordedUp != row.Up || row.RecordedDown != row.Down {
			view.Coverage = "partial"
		}
	}
	return view
}
func positiveQuery(c *gin.Context, key string, defaultValue, maximum int) (int, bool) {
	value := c.Query(key)
	if value == "" {
		return defaultValue, true
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 || n > maximum || (key != "offset" && n == 0) {
		badRequest(c, errors.New("流量来源查询参数无效"))
		return 0, false
	}
	return n, true
}

// GET /users/:id/traffic-sources. Hours are settlement hours, not client
// activity timestamps: offline batches are attributed when they are settled.
func (s *Server) userTrafficSources(c *gin.Context) {
	var user store.User
	if err := s.st.DB.Select("id", "is_service").First(&user, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if user.IsService {
		notFound(c)
		return
	}
	hours, ok := positiveQuery(c, "hours", 24, 90*24)
	if !ok {
		return
	}
	limit, ok := positiveQuery(c, "limit", 50, 200)
	if !ok {
		return
	}
	offset, ok := positiveQuery(c, "offset", 0, 1000000)
	if !ok {
		return
	}
	now := time.Now().UTC()
	from := now.Truncate(time.Hour).Add(-time.Duration(hours-1) * time.Hour)
	base := func() *gorm.DB {
		return s.st.DB.Model(&store.TrafficHourly{}).Where("user_id = ? AND hour >= ? AND hour <= ?", user.ID, from, now)
	}
	// Options stay available when a node filter is active, including deleted nodes.
	var options []struct {
		NodeID   string `json:"nodeId"`
		NodeName string `json:"nodeName"`
	}
	if err := base().Select("traffic_hourlies.node_id, COALESCE(nodes.name, '') AS node_name").Joins("LEFT JOIN nodes ON nodes.id = traffic_hourlies.node_id").Group("traffic_hourlies.node_id, nodes.name").Order("traffic_hourlies.node_id").Scan(&options).Error; err != nil {
		serverError(c, err)
		return
	}
	names := map[string]string{}
	for _, node := range options {
		names[node.NodeID] = node.NodeName
	}
	filtered := func() *gorm.DB {
		q := base()
		if node := c.Query("node"); node != "" {
			q = q.Where("node_id = ?", node)
		}
		return q
	}
	var total int64
	if err := filtered().Count(&total).Error; err != nil {
		serverError(c, err)
		return
	}
	var summary struct {
		Up             int64 `json:"up"`
		Down           int64 `json:"down"`
		RawUp          int64 `json:"rawUp"`
		RawDown        int64 `json:"rawDown"`
		AccountUp      int64 `json:"accountUp"`
		AccountDown    int64 `json:"accountDown"`
		RecordedUp     int64 `json:"recordedUp"`
		RecordedDown   int64 `json:"recordedDown"`
		IncompleteRows int64 `json:"incompleteRows"`
		RecordedRows   int64 `json:"recordedRows"`
	}
	if err := filtered().Select(`COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down,
 COALESCE(SUM(raw_up),0) AS raw_up, COALESCE(SUM(raw_down),0) AS raw_down,
 COALESCE(SUM(account_up),0) AS account_up, COALESCE(SUM(account_down),0) AS account_down,
 COALESCE(SUM(recorded_up),0) AS recorded_up, COALESCE(SUM(recorded_down),0) AS recorded_down,
 COALESCE(SUM(CASE WHEN raw_recorded = true THEN 1 ELSE 0 END),0) AS recorded_rows,
 COALESCE(SUM(CASE WHEN raw_recorded = false OR recorded_up <> up OR recorded_down <> down THEN 1 ELSE 0 END),0) AS incomplete_rows`).Scan(&summary).Error; err != nil {
		serverError(c, err)
		return
	}
	var rows []store.TrafficHourly
	if err := filtered().Order("hour desc, node_id asc").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	views := make([]userTrafficSource, 0, len(rows))
	for _, row := range rows {
		views = append(views, trafficSourceView(row, names[row.NodeID]))
	}
	if options == nil {
		options = make([]struct {
			NodeID   string `json:"nodeId"`
			NodeName string `json:"nodeName"`
		}, 0)
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"items": views, "nodes": options, "summary": summary, "total": total, "from": from, "to": now, "limit": limit, "offset": offset})
}
