package panel

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errUSDTQuoteConflict = errors.New("USDT amount reserved concurrently")

func usdtMatchUntil(o *store.Order) time.Time {
	return maxTime(o.CreatedAt, o.ExpiresAt).Add(usdtLookback)
}
func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
func normalizedUSDTAddress(chain, address string) string {
	if n, ok := usdtNetByID(chain); ok && n.EVM {
		return strings.ToLower(address)
	}
	return address
}

// Reserve and create in the same transaction. A process mutex cannot protect
// this quote when the next request lands on a different panel instance.
func (s *Server) reserveUSDTOrder(o *store.Order, price float64, now time.Time) (*store.Order, error) {
	n, ok := usdtNetByID(o.Chain)
	if !ok {
		return nil, errors.New("unknown USDT network")
	}
	o.Chain = n.ID
	o.PayAddress = s.usdtAddress(n)
	if o.PayAddress == "" {
		return nil, errors.New("USDT receiving address is not configured")
	}
	address := normalizedUSDTAddress(o.Chain, o.PayAddress)
	offset, err := rand.Int(rand.Reader, big.NewInt(500))
	if err != nil {
		return nil, err
	}
	base := int64(math.Round(price * 1e4))
	for i := int64(0); i < 500; i++ {
		units := base + (offset.Int64()+i)%500 + 1
		o.Amount = fmt.Sprintf("%d.%04d", units/10000, units%10000)
		err = s.st.DB.Transaction(func(tx *gorm.DB) error {
			if e := tx.Where("until < ?", now).Delete(&store.USDTReservation{}).Error; e != nil {
				return e
			}
			// Old orders have no frozen address/reservation. Do not reuse their
			// amounts on this network before their payment window and the
			// timestamp tolerance in matchUSDT have elapsed.
			q := tx.Model(&store.Order{}).Where("method = ? AND amount = ? AND COALESCE(pay_address, '') = '' AND (created_at >= ? OR expires_at >= ?)", payUSDT, o.Amount, now.Add(-usdtLookback-time.Minute), now.Add(-usdtLookback-time.Minute))
			if o.Chain == "tron" {
				q = q.Where("chain IN ?", []string{"", "tron"})
			} else {
				q = q.Where("chain = ?", o.Chain)
			}
			var count int64
			if e := q.Count(&count).Error; e != nil {
				return e
			}
			if count > 0 {
				return errUSDTQuoteConflict
			}
			r := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&store.USDTReservation{Chain: o.Chain, Address: address, Amount: o.Amount, OrderID: o.ID, Until: usdtMatchUntil(o).Add(time.Minute)})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected == 0 {
				return errUSDTQuoteConflict
			}
			return tx.Create(o).Error
		})
		if errors.Is(err, errUSDTQuoteConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return o, nil
	}
	return nil, errors.New("同一时间的 USDT 订单太多，请稍后再试")
}
