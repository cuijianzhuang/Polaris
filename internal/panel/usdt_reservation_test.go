package panel

import (
	"crypto/rand"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

type quoteZeros struct{}

func (quoteZeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestUSDTReservationAcrossInstances(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		for _, s := range ss {
			s.st.SetSetting("shop_usdt_address", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
		}
		previous := rand.Reader
		rand.Reader = quoteZeros{}
		defer func() { rand.Reader = previous }()
		now := time.Now()
		ready := make(chan struct{}, 2)
		release := make(chan struct{})
		if dsn != "" { // SQLite serializes writers before this read; only PG can overlap.
			for _, s := range ss {
				var once sync.Once
				if e := s.st.DB.Callback().Query().After("gorm:query").Register("test:quote-race", func(tx *gorm.DB) {
					if tx.Statement.Table == "orders" {
						once.Do(func() { ready <- struct{}{}; <-release })
					}
				}); e != nil {
					t.Fatal(e)
				}
			}
		}
		var wg sync.WaitGroup
		got := make([]*store.Order, 2)
		errs := make([]error, 2)
		for i, s := range ss {
			wg.Add(1)
			go func(i int, s *Server) {
				defer wg.Done()
				for retry := 0; retry < 30; retry++ {
					o := &store.Order{ID: fmt.Sprint("o", i), Method: payUSDT, Chain: "tron", Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
					got[i], errs[i] = s.reserveUSDTOrder(o, 2.5, now)
					if errs[i] == nil {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}(i, s)
		}
		if dsn != "" {
			for range 2 {
				select {
				case <-ready:
				case <-time.After(5 * time.Second):
					close(release)
					wg.Wait()
					t.Fatal("concurrent quote reads did not overlap")
				}
			}
		}
		close(release)
		wg.Wait()
		for _, e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
		if got[0].Amount == got[1].Amount {
			t.Fatal("duplicate quote across instances", got[0].Amount)
		}
		if e := ss[0].st.DB.Model(got[0]).Update("status", "cancelled").Error; e != nil {
			t.Fatal(e)
		}
		third, e := ss[1].reserveUSDTOrder(&store.Order{ID: "third", Method: payUSDT, Chain: "tron", Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, 2.5, now)
		if e != nil {
			t.Fatal(e)
		}
		if third.Amount == got[0].Amount || third.Amount == got[1].Amount {
			t.Fatal("cancel freed a still-payable quote")
		}
		// A failed order INSERT must roll back its quote as well.
		ss[0].st.DB.Callback().Create().Before("gorm:create").Register("test:fail-order", func(tx *gorm.DB) {
			if tx.Statement.Table == "orders" {
				tx.AddError(fmt.Errorf("injected order insert failure"))
			}
		})
		_, e = ss[0].reserveUSDTOrder(&store.Order{ID: "rollback", Method: payUSDT, Chain: "tron", CreatedAt: now}, 2.5, now)
		if e == nil {
			t.Fatal("injected order failure ignored")
		}
		var n int64
		ss[1].st.DB.Model(&store.USDTReservation{}).Where("order_id = ?", "rollback").Count(&n)
		if n != 0 {
			t.Fatal("failed order leaked reservation")
		}
		future := usdtMatchUntil(got[0]).Add(2 * time.Minute)
		released, e := ss[1].reserveUSDTOrder(&store.Order{ID: "after-window", Method: payUSDT, Chain: "tron", CreatedAt: future, ExpiresAt: future.Add(time.Hour)}, 2.5, future)
		if e != nil || released.Amount != "2.5001" {
			t.Fatalf("expired reservation not released: %+v %v", released, e)
		}
	})
}
