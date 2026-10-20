package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/provider"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestNodeProviderCalibrationFailureRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":0,"plan_monthly_data":100,"data_counter":90}`))
	}))
	defer srv.Close()
	old := provider.BWHBase
	provider.BWHBase = srv.URL
	defer func() { provider.BWHBase = old }()
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "net", Name: "net"})
	p := store.NodeProvider{NodeID: "net", Kind: providerBandwagon, VEID: "1", APIKey: s.st.Box.Seal("fixture"), Sync: true}
	s.st.DB.Create(&p)
	s.st.DB.Callback().Update().Before("gorm:update").Register("test:calibration-fail", func(tx *gorm.DB) {
		if tx.Statement.Table == "nodes" {
			tx.AddError(errors.New("calibration write failed"))
		}
	})
	if _, err := s.syncProvider(context.Background(), &p, false); err == nil {
		t.Fatal("sync acknowledged a failed calibration")
	}
	s.st.DB.Callback().Update().Remove("test:calibration-fail")
	s.st.DB.First(&p, "node_id = ?", "net")
	if !strings.Contains(p.LastError, "calibration write failed") {
		t.Fatalf("sync failure hidden from provider view: %+v", providerViewOf(&p))
	}
	if _, err := s.syncProvider(context.Background(), &p, false); err != nil {
		t.Fatal(err)
	}
	s.st.DB.First(&p, "node_id = ?", "net")
	if p.LastError != "" {
		t.Fatalf("successful retry did not clear error: %s", p.LastError)
	}
}

// A node linked to BandwagonHost follows the provider's traffic figures.
func TestNodeProviderBandwagon(t *testing.T) {
	used := `107374182400` // 100 GB raw, ×1.5 = 150 GB billed
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "k1" {
			w.Write([]byte(`{"error":700005,"message":"Authentication failure"}`))
			return
		}
		w.Write([]byte(`{"error":0,"plan":"KVM","plan_monthly_data":214748364800,"monthly_data_multiplier":1.5,"data_counter":` + used +
			`,"data_next_reset":` + itoa64(time.Date(2026, 11, 15, 8, 0, 0, 0, time.Local).Unix()) + `,"ve_status":"running"}`))
	}))
	defer srv.Close()
	old := provider.BWHBase
	provider.BWHBase = srv.URL
	defer func() { provider.BWHBase = old }()

	s, _, admin := newAdminClient(t)
	ev := events(s)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "bwh-la", NetRx: 1 << 30, NetTx: 1 << 30})

	if st, out := admin.do("PUT", "/nodes/n1/provider", map[string]any{"veid": "123", "apiKey": "bad", "sync": true}); st != 400 || !strings.Contains(out["error"].(string), "Authentication") {
		t.Fatalf("bad key saved: %d %v", st, out)
	}
	st, out := admin.do("PUT", "/nodes/n1/provider", map[string]any{"veid": "123", "apiKey": "k1", "sync": true})
	if st != 200 {
		t.Fatalf("save %d %v", st, out)
	}
	pv := out["provider"].(map[string]any)
	if pv["keySet"] != true || pv["info"].(map[string]any)["dataUsed"].(float64) != 150<<30 {
		t.Fatalf("view %v", pv)
	}
	var n store.Node
	s.st.DB.First(&n, "id = ?", "n1")
	if n.NetLimit != 200<<30 || n.NetResetDay != 15 || netUsed(&n) != 150<<30 {
		t.Fatalf("calibrated %+v used %d", n, netUsed(&n))
	}
	// 150 / 200 = 75%: under the 80% threshold, no alert yet.
	select {
	case e := <-ev:
		t.Fatalf("unexpected alert %q", e.Text)
	case <-time.After(100 * time.Millisecond):
	}
	// The next poll sees 170 GB billed: past 80%, one alert.
	used = `121762322090`
	s.syncProviders()
	s.st.DB.First(&n, "id = ?", "n1")
	if n.NetWarned != 1 || netUsed(&n) < 169<<30 {
		t.Fatalf("after sync %+v", n)
	}
	if got := next(t, ev); !strings.Contains(got, "bwh-la") {
		t.Fatalf("alert %q", got)
	}
	// The key is never returned; an empty key on save keeps it.
	if st, out := admin.do("PUT", "/nodes/n1/provider", map[string]any{"veid": "123", "sync": false}); st != 200 || strings.Contains(toJSONString(out), "k1") {
		t.Fatalf("resave %d %v", st, out)
	}
	if st, _ := admin.do("POST", "/nodes/n1/provider/refresh", nil); st != 200 {
		t.Fatal("refresh")
	}
	if st, _ := admin.do("DELETE", "/nodes/n1/provider", nil); st != 204 {
		t.Fatal("delete")
	}
	if _, out := admin.do("GET", "/nodes/n1/provider", nil); out["configured"] != false {
		t.Fatal("still configured")
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func toJSONString(v any) string { b, _ := json.Marshal(v); return string(b) }
