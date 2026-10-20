package panel

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// acceptPayment acknowledges only a durable, validated receipt. Fulfilment
// may fail independently; the persisted receipt remains the retry authority.
func (s *Server) acceptPayment(o *store.Order, ref string) error {
	if ref == "" || len(ref) > 128 || !strings.HasPrefix(ref, o.Method+":") {
		return errors.New("invalid payment reference")
	}
	now := time.Now()
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		// Serialize callbacks and cleanup before reading the order. A concurrent
		// cleaner must recheck its expired/unpaid predicate after this commits.
		r := tx.Exec("UPDATE orders SET status=status WHERE id = ?", o.ID)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		var current store.Order
		if e := tx.First(&current, "id = ?", o.ID).Error; e != nil {
			return e
		}
		if current.Method != o.Method || current.Amount != o.Amount || current.Currency != o.Currency {
			return errors.New("payment order changed")
		}
		receipt := store.PaymentReceipt{Ref: ref, OrderID: o.ID, Method: o.Method, Amount: o.Amount, Currency: o.Currency, Status: "pending", NextAt: now, CreatedAt: now, UpdatedAt: now}
		if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt).Error; e != nil {
			return e
		}
		var saved store.PaymentReceipt
		if e := tx.First(&saved, "ref = ?", ref).Error; e != nil {
			return e
		}
		if saved.OrderID != o.ID || saved.Method != o.Method || saved.Amount != o.Amount || saved.Currency != o.Currency {
			return errors.New("payment reference belongs to another order")
		}
		if current.PayRef != nil && *current.PayRef != ref {
			return tx.Model(&store.PaymentReceipt{}).Where("ref = ?", ref).Updates(map[string]any{"status": "review", "last_error": "同一订单收到额外付款，请人工对账", "updated_at": now}).Error
		}
		return tx.Model(&store.Order{}).Where("id = ? AND status IN ?", o.ID, []string{"pending", "expired", "cancelled"}).Update("status", "processing").Error
	})
	if err != nil {
		return err
	}
	s.processPaymentReceipt(ref)
	return nil
}

func (s *Server) processPaymentReceipt(ref string) {
	now := time.Now()
	token := store.NewID()
	r := s.st.DB.Model(&store.PaymentReceipt{}).Where("ref = ? AND status = ? AND next_at <= ? AND lease_until <= ?", ref, "pending", now, now).
		Updates(map[string]any{"lease_until": now.Add(time.Minute), "lease_token": token, "attempts": gorm.Expr("attempts + 1"), "updated_at": now})
	if r.Error != nil || r.RowsAffected != 1 {
		return
	}
	var job store.PaymentReceipt
	if e := s.st.DB.First(&job, "ref = ? AND lease_token = ?", ref, token).Error; e != nil {
		return
	}
	o, _, _, err := s.fulfil(job.OrderID, job.Ref, nil)
	state, detail := "done", ""
	var input commandInputError
	if err != nil {
		state, detail = "pending", "交付暂时失败，等待自动重试"
		if errors.As(err, &input) {
			state, detail = "review", truncate(err.Error(), 255)
		}
		s.log.Warn("payment delivery deferred", "order", job.OrderID, "err", err)
	} else if o.PayRef == nil || *o.PayRef != job.Ref {
		state, detail = "review", "订单已由其他付款开通，请人工对账"
	} else if o.Status != "paid" && o.Status != "refund_pending" && o.Status != "refunded" {
		state, detail = "review", "付款已记录，订单需要人工处理"
	}
	update := map[string]any{"status": state, "last_error": detail, "lease_until": time.Time{}, "lease_token": "", "updated_at": time.Now(), "next_at": now.Add(time.Duration(min(job.Attempts, 30)) * 10 * time.Second)}
	if e := s.st.DB.Model(&store.PaymentReceipt{}).Where("ref = ? AND lease_token = ?", ref, token).Updates(update).Error; e != nil {
		s.log.Error("save payment recovery result", "order", job.OrderID, "err", e)
	}
}

func (s *Server) retryPayments() {
	var jobs []store.PaymentReceipt
	if e := s.st.DB.Where("status = ? AND next_at <= ? AND lease_until <= ?", "pending", time.Now(), time.Now()).Order("created_at").Limit(50).Find(&jobs).Error; e != nil {
		s.log.Warn("load payment recovery tasks", "err", e)
		return
	}
	for _, j := range jobs {
		if !s.isLeader() {
			return
		}
		s.processPaymentReceipt(j.Ref)
	}
}

// retryOrderPayment requeues a recorded payment, never fabricates money.
func (s *Server) retryOrderPayment(id string) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		var p store.PaymentReceipt
		if e := tx.Where("order_id = ? AND status = ?", id, "review").First(&p).Error; e != nil {
			return e
		}
		// Refuse automatic delivery of an additional charge on an already paid order.
		var o store.Order
		if e := tx.First(&o, "id = ?", id).Error; e != nil {
			return e
		}
		if o.Status != "failed" || o.PayRef == nil || *o.PayRef != p.Ref {
			return fmt.Errorf("订单不处于可重试交付状态")
		}
		if e := tx.Model(&o).Updates(map[string]any{"status": "processing", "pay_ref": nil}).Error; e != nil {
			return e
		}
		return tx.Model(&p).Updates(map[string]any{"status": "pending", "next_at": time.Now(), "last_error": ""}).Error
	})
}

func (s *Server) retryOrderPaymentHandler(c *gin.Context) {
	if err := s.retryOrderPayment(c.Param("id")); err != nil {
		badRequest(c, err)
		return
	}
	var p store.PaymentReceipt
	if err := s.st.DB.Where("order_id = ? AND status = ?", c.Param("id"), "pending").First(&p).Error; err != nil {
		serverError(c, err)
		return
	}
	s.processPaymentReceipt(p.Ref)
	s.auditCtx(c, "order.delivery_retry", c.Param("id"), "")
	c.JSON(http.StatusOK, gin.H{"queued": true})
}
