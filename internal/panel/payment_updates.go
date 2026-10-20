package panel

import (
	"errors"
	"strconv"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

// The polling offset advances only after trusted money events commit.
func (b *tgBot) paymentUpdate(u tgUpdate) (bool, error) {
	m := u.Message
	if m == nil || (m.SuccessfulPayment == nil && m.RefundedPayment == nil) {
		return false, nil
	}
	if m.Chat.Type != "private" || m.Chat.ID <= 0 {
		return true, nil
	}
	if m.RefundedPayment != nil {
		return true, b.s.applyRefundUpdate(m.Chat.ID, m.RefundedPayment)
	}
	if m.From.ID != m.Chat.ID {
		return true, nil
	}
	return true, b.onStarsPaid(m.Chat.ID, m.SuccessfulPayment)
}
func (s *Server) applyRefundUpdate(chat int64, p *tgPayment) error {
	var o store.Order
	err := s.st.DB.Where("method = ? AND pay_ref = ?", payStars, "stars:"+p.ChargeID).First(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		s.log.Warn("unmatched Telegram refund", "charge", p.ChargeID)
		return nil
	}
	if err != nil {
		return err
	}
	if p.Currency != "XTR" || o.Amount != strconv.FormatInt(p.TotalAmount, 10) || o.TelegramID != chat || (p.Payload != "" && p.Payload != o.ID) {
		s.log.Warn("Telegram refund fields do not match", "order", o.ID)
		return nil
	}
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := tx.Model(&o).Where("status IN ?", []string{"paid", "refund_pending", "refunded"}).Update("status", "refunded").Error; e != nil {
			return e
		}
		return tx.Model(&store.RefundJob{}).Where("order_id = ?", o.ID).Updates(map[string]any{"status": "done", "last_error": "", "lease_token": "", "lease_until": time.Time{}, "updated_at": time.Now()}).Error
	})
}
