package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func refundFixture(t *testing.T, s *Server, handler http.HandlerFunc) *store.Order {
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	s.tg.mu.Lock()
	s.tg.token, s.tg.base, s.tg.selfID = "original-bot", api.URL, 1
	s.tg.mu.Unlock()
	o := recoveryOrder(t, s, "refund", "u", payStars)
	if _, _, _, e := s.fulfil(o.ID, "stars:charge", nil); e != nil {
		t.Fatal(e)
	}
	return o
}
func TestRefundConfirmedBeforeStorageFailure(t *testing.T) {
	s := newTestServer(t)
	var calls atomic.Int32
	o := refundFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"ok":true,"result":true}`))
	})
	s.st.DB.Callback().Create().Before("gorm:create").Register("test:refund-job", func(tx *gorm.DB) {
		if tx.Statement.Table == "refund_jobs" {
			tx.AddError(fmt.Errorf("task storage unavailable"))
		}
	})
	if e := s.requestRefund(o.ID, "admin"); e == nil || calls.Load() != 0 {
		t.Fatal("external refund called before job commit")
	}
	s.st.DB.Callback().Create().Remove("test:refund-job")
	if e := s.requestRefund(o.ID, "admin"); e != nil {
		t.Fatal(e)
	}
	s.st.DB.Callback().Update().Before("gorm:update").Register("test:refund-order", func(tx *gorm.DB) {
		if tx.Statement.Table == "orders" {
			tx.AddError(fmt.Errorf("order storage unavailable"))
		}
	})
	s.processRefund(o.ID)
	var j store.RefundJob
	s.st.DB.First(&j, "order_id = ?", o.ID)
	if j.Status != "confirmed" || calls.Load() != 1 {
		t.Fatalf("confirmation lost: %+v %d", j, calls.Load())
	}
	s.st.DB.Callback().Update().Remove("test:refund-order")
	// Simulate a restart and an unrelated newly configured bot. A confirmed
	// refund finishes from storage without issuing another network request.
	other, e := New(s.cfg, s.log, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { other.st.Close() })
	s.st.DB.Model(&j).Update("lease_until", time.Time{})
	other.processRefund(o.ID)
	var current store.Order
	other.st.DB.First(&current, "id = ?", o.ID)
	other.st.DB.First(&j, "order_id = ?", o.ID)
	if current.Status != "refunded" || j.Status != "done" || calls.Load() != 1 {
		t.Fatalf("refund did not recover: %+v %+v", current, j)
	}
	if e := other.requestRefund(o.ID, "admin"); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(j)
	if strings.Contains(string(raw), "original-bot") || strings.Contains(string(raw), "botToken") {
		t.Fatal("refund credentials exposed")
	}
}
func TestRefundUnknownResultReconcilesOriginalBot(t *testing.T) {
	s := newTestServer(t)
	var refundCalls atomic.Int32
	var buyer atomic.Int64
	buyer.Store(999)
	o := refundFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/botoriginal-bot/") {
			t.Error("wrong bot used", r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "refundStarPayment") {
			refundCalls.Add(1)
			w.Write([]byte(`{"ok":false,"description":"temporary failure"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"transactions": []any{map[string]any{"id": "charge", "amount": 10, "receiver": map[string]any{"type": "user", "transaction_type": "invoice_payment", "user": map[string]any{"id": buyer.Load()}, "invoice_payload": "refund"}}}}})
	})
	if e := s.requestRefund(o.ID, "admin"); e != nil {
		t.Fatal(e)
	}
	s.processRefund(o.ID)
	s.tg.mu.Lock()
	s.tg.token = "another-bot"
	s.tg.mu.Unlock()
	retry := func() {
		s.st.DB.Model(&store.RefundJob{}).Where("order_id = ?", o.ID).Updates(map[string]any{"next_at": time.Now().Add(-time.Second), "lease_until": time.Time{}})
		s.processRefund(o.ID)
	}
	retry()
	var cur store.Order
	s.st.DB.First(&cur, "id = ?", o.ID)
	if cur.Status != "refund_pending" || refundCalls.Load() != 1 {
		t.Fatal("wrong buyer falsely confirmed/reissued refund")
	}
	buyer.Store(700)
	retry()
	s.st.DB.First(&cur, "id = ?", o.ID)
	if cur.Status != "refunded" || refundCalls.Load() != 1 {
		t.Fatal("confirmed refund was not reconciled")
	}
	// Repeated Telegram money events must not reverse a different buyer/order.
	p := &tgPayment{ChargeID: "charge", Currency: "XTR", TotalAmount: 10, Payload: o.ID}
	if e := s.applyRefundUpdate(999, p); e != nil {
		t.Fatal(e)
	}
	s.st.DB.Model(&store.RefundJob{}).Where("order_id = ?", o.ID).Updates(map[string]any{"status": "confirming", "lease_token": "stale-worker"})
	if e := s.applyRefundUpdate(700, p); e != nil {
		t.Fatal(e)
	}
	s.deferRefund(store.RefundJob{OrderID: o.ID, LeaseToken: "stale-worker"}, "confirming", "stale network result")
	var job store.RefundJob
	s.st.DB.First(&job, "order_id = ?", o.ID)
	if job.Status != "done" || job.LeaseToken != "" {
		t.Fatal("stale worker reopened a confirmed refund", job)
	}
}
func TestRefundConcurrentRequests(t *testing.T) {
	s := newTestServer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var n atomic.Int32
	o := refundFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		close(entered)
		<-release
		w.Write([]byte(`{"ok":true,"result":true}`))
	})
	if e := s.requestRefund(o.ID, "admin"); e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go func() { s.processRefund(o.ID); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refund not requested")
	}
	if e := s.requestRefund(o.ID, "admin"); e != nil {
		t.Fatal(e)
	}
	s.processRefund(o.ID)
	releaseOnce.Do(func() { close(release) })
	<-done
	if n.Load() != 1 {
		t.Fatal("concurrent refund requested twice")
	}
}
