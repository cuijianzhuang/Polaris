package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"
)

func (s *Server) recordOperationalResult(key, detail string, err error) {
	now := time.Now()
	st := store.OperationalStatus{Key: key, FirstSeen: now, UpdatedAt: now, Failed: err != nil}
	updates := map[string]any{"failed": st.Failed, "updated_at": now, "detail": ""}
	if err != nil {
		st.Detail = detail
		st.LastFailure = &now
		updates["detail"] = detail
		updates["last_failure"] = now
	} else {
		st.LastSuccess = &now
		updates["last_success"] = now
	}
	if e := s.st.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.Assignments(updates)}).Create(&st).Error; e != nil {
		s.log.Warn("operational status not persisted", "key", key)
	}
}
func (s *Server) observeTrafficQueue(node, name string, q *polarisv1.TrafficQueueStatus) {
	if q == nil {
		return
	} // Older agents do not report a health result.
	var err error
	detail := ""
	switch {
	case q.PersistenceFailed:
		err = errors.New("queue persistence failed")
		detail = "Agent 流量队列无法写盘；请先修复磁盘，避免强制重启"
	case q.DroppedBatches > 0:
		err = errors.New("queue dropped traffic")
		detail = fmt.Sprintf("Agent 本次运行已丢弃 %d 个流量批次", q.DroppedBatches)
	case q.PendingBatches > 0 && q.OldestUnackedAt > 0 && time.Since(time.Unix(q.OldestUnackedAt, 0)) > 15*time.Minute:
		err = errors.New("queue backlog")
		detail = fmt.Sprintf("Agent 有 %d 个流量批次积压超过 15 分钟", q.PendingBatches)
	}
	if detail != "" {
		detail = "节点 " + name + "：" + detail
	}
	s.recordOperationalResult("agent-traffic:"+node, detail, err)
}

type operationalIssue struct {
	Key    string `json:"key"`
	Detail string `json:"detail"`
}

func (s *Server) operationalIssues() ([]operationalIssue, error) {
	issues := []operationalIssue{}
	var states []store.OperationalStatus
	if e := s.st.DB.Find(&states).Error; e != nil {
		return nil, e
	}
	for _, st := range states {
		if st.Failed {
			issues = append(issues, operationalIssue{st.Key, st.Detail})
		}
	}
	var pending, review, refunds, cluster int64
	queries := []struct {
		name  string
		count *int64
	}{
		{"payment-pending", &pending}, {"payment-review", &review}, {"refund-pending", &refunds}, {"cluster-pending", &cluster},
	}
	for _, q := range queries {
		var err error
		switch q.name {
		case "payment-pending":
			err = s.st.DB.Model(&store.PaymentReceipt{}).Where("status = ? AND created_at < ?", "pending", time.Now().Add(-time.Minute)).Count(q.count).Error
		case "payment-review":
			err = s.st.DB.Model(&store.PaymentReceipt{}).Where("status = ?", "review").Count(q.count).Error
		case "refund-pending":
			err = s.st.DB.Model(&store.RefundJob{}).Where("status <> ? AND created_at < ?", "done", time.Now().Add(-time.Minute)).Count(q.count).Error
		case "cluster-pending":
			err = s.st.DB.Model(&store.ClusterDelivery{}).Where("status = ? AND operation_id IN (?)", "pending", s.st.DB.Model(&store.ClusterOperation{}).Select("id").Where("created_at < ?", time.Now().Add(-time.Minute))).Count(q.count).Error
		}
		if err != nil {
			return nil, err
		}
	}
	for _, v := range []struct {
		key, label string
		n          int64
	}{{"payment-pending", "付款已记录但交付仍在重试", pending}, {"payment-review", "付款需要人工对账", review}, {"refund-pending", "退款仍在处理或核验", refunds}, {"cluster-pending", "集群操作仍未受理，请检查离线实例或更新组件", cluster}} {
		if v.n > 0 {
			issues = append(issues, operationalIssue{v.key, fmt.Sprintf("%s：%d 笔", v.label, v.n)})
		}
	}
	var inbox int64
	if e := s.st.DB.Model(&store.TrafficInbox{}).Where("up > 0 OR down > 0").Count(&inbox).Error; e != nil {
		return nil, e
	}
	for _, st := range states {
		if st.Key == "traffic-settlement" && inbox > 0 && (st.LastSuccess == nil || time.Since(*st.LastSuccess) > time.Minute) {
			issues = append(issues, operationalIssue{"traffic-backlog", "存在待结算流量，超过 1 分钟未成功结算"})
		}
	}
	schedule := s.st.GetSetting("backup_schedule", "off")
	if schedule == "daily" || schedule == "weekly" {
		var st store.OperationalStatus
		r := s.st.DB.Where("key = ?", "backup").First(&st)
		if r.Error != nil {
			now := time.Now()
			s.warnWrite("backup status", s.st.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&store.OperationalStatus{Key: "backup", FirstSeen: now, UpdatedAt: now}).Error)
		} else {
			last := st.FirstSeen
			if st.LastSuccess != nil {
				last = *st.LastSuccess
			}
			limit := 26 * time.Hour
			if schedule == "weekly" {
				limit = 8 * 24 * time.Hour
			}
			if time.Since(last) > limit {
				issues = append(issues, operationalIssue{"backup-overdue", "定时备份已超过预期时间，请检查最近成功备份"})
			}
		}
	}
	return issues, nil
}
func (s *Server) checkOperationalHealth() {
	issues, err := s.operationalIssues()
	if err != nil {
		s.log.Error("operational checks unavailable", "err", err)
		return
	}
	for _, i := range issues {
		if s.shared.once("operational:"+i.Key, 30*time.Minute) {
			s.notify(Event{Kind: "operational_alert", ResourceID: i.Key, Text: "⚠️ " + i.Detail, Data: map[string]any{"key": i.Key}})
		}
	}
}
func (s *Server) getOperationalHealth(c *gin.Context) {
	issues, err := s.operationalIssues()
	if err != nil {
		serverError(c, err)
		return
	}
	var states []store.OperationalStatus
	if e := s.st.DB.Order("key").Find(&states).Error; e != nil {
		serverError(c, e)
		return
	}
	c.JSON(http.StatusOK, gin.H{"issues": issues, "states": states, "checkedAt": time.Now()})
}

// This authenticated endpoint intentionally reveals no public service identity.
func (s *Server) readiness(c *gin.Context) {
	db, e := s.st.DB.DB()
	if e != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	if e = db.PingContext(ctx); e != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ready": true})
}
