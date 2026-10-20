package panel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func (s *Server) requestRefund(id, who string) error {
	var o store.Order
	if e := s.st.DB.First(&o, "id = ?", id).Error; e != nil {
		return e
	}
	if o.Method == payStars && (o.Status == "refund_pending" || o.Status == "refunded") {
		return nil
	}
	if o.Method != payStars || o.Status != "paid" || o.PayRef == nil || !strings.HasPrefix(*o.PayRef, "stars:") {
		return errors.New("只能退款已支付的 Telegram Stars 订单")
	}
	token, base := s.tg.creds()
	if token == "" {
		return errors.New("请先连接订单所属的 Telegram 机器人")
	}
	s.tg.mu.Lock()
	botID := s.tg.selfID
	s.tg.mu.Unlock()
	if o.StarsBotID != 0 && o.StarsBotID != botID {
		return errors.New("请连接签发此订单的原机器人后重试")
	}
	now := time.Now()
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		r := tx.Model(&store.Order{}).Where("id = ? AND status = ? AND pay_ref = ?", id, "paid", *o.PayRef).Update("status", "refund_pending")
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 0 {
			return nil
		}
		j := store.RefundJob{OrderID: id, Ref: *o.PayRef, Buyer: o.TelegramID, BotToken: s.st.Box.Seal(token), APIBase: base, Status: "pending", NextAt: now, CreatedAt: now, UpdatedAt: now}
		if e := tx.Create(&j).Error; e != nil {
			return e
		}
		return tx.Create(&store.AuditLog{Admin: who, Action: "order.refund_request", Target: id, Detail: o.Amount + " XTR"}).Error
	})
}

func (s *Server) processRefund(id string) {
	now := time.Now()
	token := store.NewID()
	r := s.st.DB.Model(&store.RefundJob{}).Where("order_id = ? AND status <> ? AND next_at <= ? AND lease_until <= ?", id, "done", now, now).
		Updates(map[string]any{"lease_token": token, "lease_until": now.Add(2 * time.Minute), "attempts": gorm.Expr("attempts + 1"), "updated_at": now})
	if r.Error != nil || r.RowsAffected != 1 {
		return
	}
	var j store.RefundJob
	if s.st.DB.First(&j, "order_id = ? AND lease_token = ?", id, token).Error != nil {
		return
	}
	if j.Status == "confirmed" {
		s.finishRefund(j)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	botToken := s.st.Box.MustOpen(j.BotToken)
	if botToken == "" {
		s.deferRefund(j, "confirming", "原机器人凭据不可用，请恢复配置后重试")
		return
	}
	if j.Status == "requesting" || j.Status == "confirming" {
		found, e := s.refundOnTelegram(ctx, j, botToken)
		if e != nil {
			s.deferRefund(j, "confirming", "退款结果待核验，Telegram 流水尚未完整获取")
			return
		}
		if found {
			s.confirmRefund(j)
			return
		}
		// A complete scan found no refund. Retry the same original charge ID;
		// Telegram refunds one payment once, even if a prior reply was lost.
	}
	r = s.st.DB.Model(&store.RefundJob{}).Where("order_id = ? AND lease_token = ?", id, token).Update("status", "requesting")
	if r.Error != nil || r.RowsAffected != 1 {
		return
	}
	var confirmed bool
	requestCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	err := s.tg.callAs(requestCtx, botToken, j.APIBase, "refundStarPayment", map[string]any{"user_id": j.Buyer, "telegram_payment_charge_id": strings.TrimPrefix(j.Ref, "stars:")}, &confirmed)
	stop()
	if err != nil || !confirmed {
		// Exact provider acknowledgement is authoritative; arbitrary API/network
		// errors must never turn an uncertain result into a completed refund.
		if err != nil && err.Error() == "telegram: CHARGE_ALREADY_REFUNDED" {
			s.confirmRefund(j)
			return
		}
		s.deferRefund(j, "confirming", "退款请求结果不明，等待 Telegram 流水核验")
		return
	}
	s.confirmRefund(j)
}

func (s *Server) confirmRefund(j store.RefundJob) {
	r := s.st.DB.Model(&store.RefundJob{}).Where("order_id = ? AND lease_token = ?", j.OrderID, j.LeaseToken).Update("status", "confirmed")
	if r.Error != nil || r.RowsAffected != 1 {
		return
	}
	s.finishRefund(j)
}
func (s *Server) finishRefund(j store.RefundJob) {
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		// Both records commit together. Confirmed tasks can finish after restart
		// without calling Telegram a second time.
		r := tx.Model(&store.RefundJob{}).Where("order_id = ? AND lease_token = ? AND status = ?", j.OrderID, j.LeaseToken, "confirmed").
			Updates(map[string]any{"status": "done", "last_error": "", "lease_until": time.Time{}, "lease_token": "", "updated_at": time.Now()})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 0 {
			return nil
		}
		r = tx.Model(&store.Order{}).Where("id = ? AND pay_ref = ? AND status IN ?", j.OrderID, j.Ref, []string{"refund_pending", "refunded"}).Update("status", "refunded")
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return errors.New("refund order changed; review required")
		}
		return tx.Create(&store.AuditLog{Admin: "system", Action: "order.refund", Target: j.OrderID, Detail: "Telegram 已确认退款"}).Error
	})
	if err != nil {
		s.log.Error("refund confirmation not committed", "order", j.OrderID)
		return
	}
}
func (s *Server) deferRefund(j store.RefundJob, state, detail string) {
	s.st.DB.Model(&store.RefundJob{}).Where("order_id = ? AND lease_token = ?", j.OrderID, j.LeaseToken).
		Updates(map[string]any{"status": state, "last_error": detail, "next_at": time.Now().Add(time.Minute), "lease_until": time.Time{}, "lease_token": "", "updated_at": time.Now()})
}
func (s *Server) retryRefunds() {
	var jobs []store.RefundJob
	if e := s.st.DB.Where("status <> ? AND next_at <= ? AND lease_until <= ?", "done", time.Now(), time.Now()).Order("created_at").Limit(20).Find(&jobs).Error; e != nil {
		return
	}
	for _, j := range jobs {
		if !s.isLeader() {
			return
		}
		s.processRefund(j.OrderID)
	}
}

type starPartner struct {
	Type            string `json:"type"`
	TransactionType string `json:"transaction_type"`
	User            struct {
		ID int64 `json:"id"`
	} `json:"user"`
	Payload string `json:"invoice_payload"`
}
type starTransaction struct {
	ID       string       `json:"id"`
	Amount   int64        `json:"amount"`
	Nano     int64        `json:"nanostar_amount"`
	Receiver *starPartner `json:"receiver"`
}

func (s *Server) refundOnTelegram(ctx context.Context, j store.RefundJob, token string) (bool, error) {
	var o store.Order
	if e := s.st.DB.First(&o, "id = ?", j.OrderID).Error; e != nil {
		return false, e
	}
	amount, e := strconv.ParseInt(o.Amount, 10, 64)
	if e != nil || amount <= 0 {
		return false, errors.New("invalid Stars amount")
	}
	for offset := 0; offset < 10000; offset += 90 {
		var result struct {
			Transactions []starTransaction `json:"transactions"`
		}
		if e := s.tg.callAs(ctx, token, j.APIBase, "getStarTransactions", map[string]any{"offset": offset, "limit": 100}, &result); e != nil {
			return false, e
		}
		for _, t := range result.Transactions {
			p := t.Receiver
			if t.ID != strings.TrimPrefix(j.Ref, "stars:") || p == nil {
				continue
			}
			if p.Type != "user" || p.TransactionType != "invoice_payment" || p.User.ID != j.Buyer || (p.Payload != "" && p.Payload != j.OrderID) || t.Nano != 0 || (t.Amount != amount && t.Amount != -amount) {
				return false, fmt.Errorf("refund transaction requires review")
			}
			return true, nil
		}
		if len(result.Transactions) < 100 {
			return false, nil
		}
	}
	return false, errors.New("refund history exceeds reconciliation window")
}
