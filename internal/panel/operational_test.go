package panel

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

func TestOperationalFailuresRecoverAndRemainVisible(t *testing.T) {
	s := newTestServer(t)
	s.observeTrafficQueue("node", "Tokyo", &polarisv1.TrafficQueueStatus{PendingBatches: 3, PersistenceFailed: true})
	s.observeTrafficQueue("node", "Tokyo", nil) // old agents cannot falsely clear failures
	var st store.OperationalStatus
	s.st.DB.First(&st, "key = ?", "agent-traffic:node")
	if !st.Failed || st.LastFailure == nil {
		t.Fatal("lost Agent queue failure")
	}
	s.recordOperationalResult("usdt-scan", "链上扫描失败", errors.New("secret upstream error"))
	old := time.Now().Add(-2 * time.Minute)
	s.st.DB.Create(&store.PaymentReceipt{Ref: "epay:x", Status: "pending", CreatedAt: old})
	s.st.DB.Create(&store.RefundJob{OrderID: "r", Status: "confirming", CreatedAt: old})
	issues, e := s.operationalIssues()
	if e != nil {
		t.Fatal(e)
	}
	keys := map[string]bool{}
	for _, i := range issues {
		keys[i.Key] = true
		if i.Detail == "secret upstream error" {
			t.Fatal("raw secret leaked")
		}
	}
	for _, k := range []string{"agent-traffic:node", "usdt-scan", "payment-pending", "refund-pending"} {
		if !keys[k] {
			t.Fatal("missing fault", k)
		}
	}
	s.observeTrafficQueue("node", "Tokyo", &polarisv1.TrafficQueueStatus{})
	s.st.DB.First(&st, "key = ?", "agent-traffic:node")
	if st.Failed || st.LastSuccess == nil || st.LastFailure == nil {
		t.Fatal("recovery lost failure/success history", st)
	}
}

func TestOperationalReadinessRequiresDatabase(t *testing.T) {
	s, _, admin := newAdminClient(t)
	if code, _ := admin.do("GET", "/ready", nil); code != 200 {
		t.Fatal(code)
	}
	if code, out := admin.do("GET", "/operational-health", nil); code != 200 || out["issues"] == nil {
		t.Fatal(code, out)
	}
	s.st.Close()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/ready", nil)
	s.readiness(c)
	c.Writer.WriteHeaderNow()
	if w.Code != 503 {
		t.Fatal("closed DB reported ready", w.Code)
	}
}
