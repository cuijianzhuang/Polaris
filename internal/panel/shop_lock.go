package panel

import (
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func lockOrderUser(tx *gorm.DB, o *store.Order, u *store.User) error {
	type lookup struct {
		column string
		value  any
	}
	var lookups []lookup
	if o.UserID != "" {
		lookups = append(lookups, lookup{"id", o.UserID})
	}
	if o.TelegramID != 0 {
		lookups = append(lookups, lookup{"telegram_id", o.TelegramID})
	}
	for _, q := range lookups {
		// Column names are internal constants. Lock before both the user read
		// and GORM's auditing hooks, as with administrator user commands.
		r := tx.Exec("UPDATE users SET updated_at = updated_at WHERE "+q.column+" = ? AND is_service = ?", q.value, false)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected > 0 {
			return tx.Where(q.column+" = ? AND is_service = ?", q.value, false).First(u).Error
		}
	}
	return gorm.ErrRecordNotFound
}
