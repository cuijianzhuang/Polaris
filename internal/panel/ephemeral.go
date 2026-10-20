package panel

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// ephStore keeps short-lived values (sign-in codes, OAuth states, callback
// links) in the database, sealed, so a code issued by one panel instance can
// be redeemed on another. Expired rows are ignored and swept.
type ephStore struct {
	db  *gorm.DB
	box *secret.Box
}

// ephAction is what an update callback decides.
type ephAction int

const (
	ephKeep   ephAction = iota // leave the row as it was
	ephSave                    // store the modified value
	ephDelete                  // remove the row
)

func (e *ephStore) put(key string, v any, ttl time.Duration) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return e.db.Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&store.EphemeralKV{Name: key, Value: e.box.Seal(string(raw)), ExpiresAt: time.Now().Add(ttl)}).Error
}

func (e *ephStore) load(tx *gorm.DB, key string, v any, lock bool) (*store.EphemeralKV, bool) {
	q := tx.Where("name = ? AND expires_at > ?", key, time.Now())
	if lock && tx.Dialector.Name() == "postgres" {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row store.EphemeralKV
	if q.Take(&row).Error != nil {
		return nil, false
	}
	raw, err := e.box.Open(row.Value)
	if err != nil || json.Unmarshal([]byte(raw), v) != nil {
		return nil, false
	}
	return &row, true
}

// get reads a value without consuming it.
func (e *ephStore) get(key string, v any) bool {
	_, ok := e.load(e.db, key, v, false)
	return ok
}

// take reads and deletes a value; of two instances taking the same key at
// once, only one gets it.
func (e *ephStore) take(key string, v any) bool {
	if _, ok := e.load(e.db, key, v, false); !ok {
		e.db.Where("name = ?", key).Delete(&store.EphemeralKV{}) // expired: single use all the same
		return false
	}
	r := e.db.Where("name = ?", key).Delete(&store.EphemeralKV{})
	return r.Error == nil && r.RowsAffected == 1
}

// update loads key into v under a row lock and applies fn's decision. It
// reports whether the key existed (and was not expired).
func (e *ephStore) update(key string, v any, fn func() ephAction) bool {
	found := false
	_ = e.db.Transaction(func(tx *gorm.DB) error {
		row, ok := e.load(tx, key, v, true)
		if !ok {
			return nil
		}
		found = true
		switch fn() {
		case ephSave:
			raw, err := json.Marshal(v)
			if err != nil {
				return err
			}
			return tx.Model(&store.EphemeralKV{}).Where("name = ?", key).Update("value", e.box.Seal(string(raw))).Error
		case ephDelete:
			return tx.Delete(row).Error
		}
		return nil
	})
	return found
}

// count returns the live keys starting with prefix.
func (e *ephStore) count(prefix string) int64 {
	var n int64
	e.db.Model(&store.EphemeralKV{}).Where("name LIKE ? AND expires_at > ?", prefix+"%", time.Now()).Count(&n)
	return n
}

// sweep drops expired rows.
func (e *ephStore) sweep() {
	e.db.Where("expires_at <= ?", time.Now()).Delete(&store.EphemeralKV{})
}
