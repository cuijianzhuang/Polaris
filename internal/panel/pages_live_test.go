package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestLiveAdminPagesBrowser opens every admin page of the built UI in a
// real browser against a real server with some data, at desktop and phone
// width and in English, and fails on a crashed page, an uncaught error or a
// server error. Needs `npm run build`, Python Playwright and Chromium.
func TestLiveAdminPagesBrowser(t *testing.T) {
	if os.Getenv("POLARIS_PAGES_BROWSER_TEST") == "" {
		t.Skip("POLARIS_PAGES_BROWSER_TEST required")
	}
	s, _, _ := newAdminClient(t)
	dir, err := filepath.Abs("../../web/dist")
	if err != nil {
		t.Fatal(err)
	}
	s.webFS = http.Dir(dir)
	srv := httptest.NewServer(s.Router())
	defer srv.Close()
	s.cfg.PublicURL = srv.URL

	// Enough data that list pages render rows, not only empty states.
	db := s.st.DB
	now := time.Now()
	db.Create(&store.Node{ID: "n1", Name: "东京", Address: "203.0.113.5", Status: "online", LastSeen: &now, Region: "JP", Flag: "JP",
		System: store.NewJSON(store.SystemInfo{CPU: 12, Mem: 40, RxBps: 1 << 20, TxBps: 2 << 20})})
	db.Create(&store.Node{ID: "n2", Name: "香港", Address: "203.0.113.6", Status: "offline"})
	db.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27", Status: "running"})
	db.Create(&store.Inbound{ID: "in1", NodeID: "n1", Tag: "reality", Port: 443, Core: model.CoreXray, Enabled: true,
		Protocol:  store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}),
		Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}),
		Security:  store.NewJSON(model.SecuritySpec{Type: model.SecReality, ServerName: "www.example.com"})})
	db.Create(&store.Group{ID: "g1", Name: "默认"})
	db.Create(&store.GroupInbound{GroupID: "g1", InboundID: "in1"})
	exp := now.Add(30 * 24 * time.Hour)
	db.Create(&store.User{ID: "u1", Username: "alice", UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", Password: "p", Status: "active",
		SubToken: "alice-subscription-token", TrafficLimit: 100 << 30, UsedDown: 3 << 30, ExpireAt: &exp})
	db.Create(&store.UserGroup{UserID: "u1", GroupID: "g1"})
	db.Create(&store.Order{ID: "o1", UserID: "u1", PlanID: "g1", Amount: "10", Currency: "CNY", Status: "paid", PaidAt: &now})
	db.Create(&store.Ticket{No: 1, UserID: "u1", Subject: "无法连接", Status: "open"})
	db.Create(&store.CDNProfile{ID: "cf", Name: "cf-main", OriginDomain: "cdn.example.com", Ports: store.NewJSON([]int{443}),
		PreferredIPs: store.NewJSON([]string{"104.16.0.1"})})

	cmd := exec.CommandContext(t.Context(), "python3", "../../web/scripts/admin-pages-live.py")
	cmd.Env = append(os.Environ(), "POLARIS_PAGES_TEST_URL="+srv.URL, "POLARIS_PAGES_TEST_SUB="+srv.URL+"/sub/alice-subscription-token")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, out)
	}
	t.Log(string(out))
}
