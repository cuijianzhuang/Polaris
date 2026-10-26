package panel

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// External node metrics: POST /nodes/:id/metrics (scope nodes:metrics:write)
// lets a monitoring plugin report custom readings such as "latency from
// Shanghai Telecom". They are stored per source and shown as such; they
// cannot touch the agent's heartbeat, CPU / memory, traffic or online state.

const (
	extMetricsPerCall = 20
	extMetricsPerNode = 100
	extMetricsKeep    = 30 * 24 * time.Hour
)

var (
	extMetricNameRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,39}$`)
	extMetricUnits  = []string{"ms", "s", "Mbps", "kbps", "%", "count", "bool"}
	// Names the agent's own figures use: an outside report must not look like one.
	extMetricReserved = []string{"cpu", "mem", "memory", "rx", "tx", "load", "online", "status", "heartbeat", "traffic", "up", "down"}
	errMetricLimit    = errors.New("这个节点的外部指标已达上限（100 个）")
)

type extMetricIn struct {
	Name  string   `json:"name"`
	Label string   `json:"label"`
	Unit  string   `json:"unit"`
	Value *float64 `json:"value"`
	At    string   `json:"at"` // RFC 3339; empty = now
}

func checkExtMetric(m extMetricIn, now time.Time) (time.Time, error) {
	switch {
	case !extMetricNameRe.MatchString(m.Name):
		return time.Time{}, fmt.Errorf("指标名 %q 应为小写字母开头，最长 40 个字符", m.Name)
	case slices.Contains(extMetricReserved, m.Name):
		return time.Time{}, fmt.Errorf("指标名 %q 保留给节点自身的数据", m.Name)
	case !slices.Contains(extMetricUnits, m.Unit):
		return time.Time{}, fmt.Errorf("指标 %s 的单位应为 %s 之一", m.Name, strings.Join(extMetricUnits, "、"))
	case m.Value == nil || math.IsNaN(*m.Value) || math.IsInf(*m.Value, 0) || math.Abs(*m.Value) > 1e12:
		return time.Time{}, fmt.Errorf("指标 %s 的数值无效", m.Name)
	case m.Unit == "bool" && *m.Value != 0 && *m.Value != 1:
		return time.Time{}, fmt.Errorf("指标 %s 的单位是 bool，数值只能是 0 或 1", m.Name)
	case utf8.RuneCountInString(m.Label) > 40 || strings.ContainsFunc(m.Label, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return time.Time{}, fmt.Errorf("指标 %s 的 label 最长 40 个字符，不能有控制字符", m.Name)
	}
	if m.At == "" {
		return now, nil
	}
	at, err := time.Parse(time.RFC3339, m.At)
	if err != nil || at.Before(now.Add(-24*time.Hour)) || at.After(now.Add(5*time.Minute)) {
		return time.Time{}, fmt.Errorf("指标 %s 的时间应为最近 24 小时内的 RFC 3339 时间", m.Name)
	}
	return at.UTC(), nil
}

// metricSource names who reports: a plugin's own token, another token, or
// the admin.
func metricSource(c *gin.Context) string {
	if t := apiTokenOf(c); t != nil {
		if t.PluginID != "" {
			return t.PluginID
		}
		return "token:" + t.ID
	}
	return "admin"
}

// postNodeMetrics is POST /nodes/:id/metrics {metrics: [...]}.
func (s *Server) postNodeMetrics(c *gin.Context) {
	var n store.Node
	if s.st.DB.Select("id").First(&n, "id = ?", c.Param("id")).Error != nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
		return
	}
	var req struct {
		Metrics []extMetricIn `json:"metrics"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if len(req.Metrics) == 0 || len(req.Metrics) > extMetricsPerCall {
		badRequest(c, fmt.Errorf("每次提交 1–%d 个指标", extMetricsPerCall))
		return
	}
	now := time.Now().UTC()
	source := metricSource(c)
	sourceLabel := source
	if token := apiTokenOf(c); token != nil {
		sourceLabel = token.Name
	}
	rows := make([]store.NodeExternalMetric, 0, len(req.Metrics))
	seen := map[string]bool{}
	for _, m := range req.Metrics {
		at, err := checkExtMetric(m, now)
		if err == nil && seen[m.Name] {
			err = fmt.Errorf("指标 %s 重复", m.Name)
		}
		if err != nil {
			badRequest(c, err)
			return
		}
		seen[m.Name] = true
		rows = append(rows, store.NodeExternalMetric{NodeID: n.ID, Source: source, SourceLabel: sourceLabel, Name: m.Name, Label: strings.TrimSpace(m.Label),
			Unit: m.Unit, Value: *m.Value, At: at, UpdatedAt: now})
	}
	var stored int64
	err := s.st.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		// Serialize quota checks for the same node on both SQLite and PostgreSQL.
		lock := tx.Exec("UPDATE nodes SET updated_at = updated_at WHERE id = ?", n.ID)
		if lock.Error != nil {
			return lock.Error
		}
		if lock.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		var have int64
		if err := tx.Model(&store.NodeExternalMetric{}).Where("node_id = ? AND NOT (source = ? AND name IN ?)", n.ID, source, mapKeys(seen)).Count(&have).Error; err != nil {
			return err
		}
		if int(have)+len(rows) > extMetricsPerNode {
			return errMetricLimit
		}
		r := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "node_id"}, {Name: "source"}, {Name: "name"}},
			DoUpdates: clause.AssignmentColumns([]string{"source_label", "label", "unit", "value", "at", "updated_at"}),
			Where:     clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "excluded.at > node_external_metrics.at"}}},
		}).Create(&rows)
		stored = r.RowsAffected
		return r.Error
	})
	if err != nil {
		if errors.Is(err, errMetricLimit) {
			badRequest(c, err)
			return
		}
		dbError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "stored": stored, "ignored": int64(len(rows)) - stored})
}

// listNodeMetrics is GET /nodes/:id/metrics/external.
func (s *Server) listNodeMetrics(c *gin.Context) {
	var out []store.NodeExternalMetric
	s.st.DB.Where("node_id = ? AND at > ?", c.Param("id"), time.Now().Add(-extMetricsKeep)).Order("source, name").Find(&out)
	if out == nil {
		out = []store.NodeExternalMetric{}
	}
	c.JSON(http.StatusOK, gin.H{"metrics": out})
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
