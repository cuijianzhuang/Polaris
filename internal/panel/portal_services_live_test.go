package panel

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestLivePortalServicesBrowser(t *testing.T) {
	if os.Getenv("POLARIS_PORTAL_SERVICES_BROWSER_TEST") == "" {
		t.Skip("POLARIS_PORTAL_SERVICES_BROWSER_TEST required")
	}
	if !proBuild {
		t.Skip("checkout browser test requires -tags pro")
	}
	s, srv, admin, plan := paymentSwitchFixture(t)
	s.cfg.PublicURL = srv.URL
	s.webFS = http.Dir("../../web/public")
	useMail(t, s)
	s.st.SetSetting("site_title", "Polaris")
	s.st.SetSetting("tickets_enabled", "true")
	s.st.SetSetting("ticket_max_open", "100")
	s.st.SetSetting("shop_auto_create", "true")
	s.st.SetSetting("shop_terms", "购买前请确认套餐与账号；开通后请妥善保管订阅。")
	s.shopLimit = &sharedLimiter{s: s.shared, name: "service-browser-orders", perMinute: 600000}
	s.ticketLimit = &sharedLimiter{s: s.shared, name: "service-browser-tickets", perMinute: 600000}
	paidAt := time.Now()
	expire := time.Now().Add(30 * 24 * time.Hour)
	u := newUser(&userReq{Username: "alice"})
	u.Email = "alice@example.com"
	u.PlanID = plan.ID
	u.ExpireAt = &expire
	u.FwdMax = 2
	other := newUser(&userReq{Username: "bob"})
	other.Email = "bob@example.com"
	for _, value := range []any{
		&u, &other,
		&store.Group{ID: "yearly-plan", Name: "专业版年度套餐-with-a-long-descriptive-name", Plan: true, Sale: true, Days: 365, TrafficLimit: 500 << 30, PriceCNY: 100, PriceUSDT: 15},
		&store.Group{ID: "monthly-plan", Name: "轻量月付", Plan: true, Sale: true, Days: 30, TrafficLimit: 50 << 30, PriceCNY: 20, PriceUSDT: 3},
		&store.Group{ID: "code-only-plan", Name: "兑换专用套餐", Plan: true, Sale: true, Days: 30, TrafficLimit: 10 << 30},
		&store.Coupon{Code: "SAVE10", Percent: 10, Plans: true, Enabled: true},
		&store.Order{ID: "ownpaidorder", UserID: u.ID, PlanID: plan.ID, PlanName: "专业版月付", Method: payUSDT, Amount: "1", Currency: "USDT", Status: "paid", PaidAt: &paidAt, ExpiresAt: expire},
		&store.Order{ID: "privateorder", UserID: other.ID, PlanID: plan.ID, PlanName: "PRIVATE-OTHER-PURCHASE", Method: payUSDT, Amount: "1", Currency: "USDT", Status: "paid", WebKey: webKeyHash(strings.Repeat("c", 32)), ExpiresAt: expire},
		&store.Ticket{ID: "own-ticket", No: 1, UserID: u.ID, Subject: "原有支持记录", Status: ticketAnswered, UserUnread: true, UpdatedAt: time.Now()},
		&store.TicketMessage{ID: "own-msg", TicketID: "own-ticket", Admin: true, Body: "请更新订阅后重试", CreatedAt: time.Now()},
		&store.Ticket{ID: "private-ticket", No: 2, UserID: other.ID, Subject: "PRIVATE-OTHER-TICKET", Status: ticketOpen},
	} {
		if err := s.st.DB.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	token, err := s.issuePortalMailToken(&u)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "python3", "../../web/scripts/portal-services-live.py")
	// The admin token is test-only and used to confirm a payment via the real
	// fulfilment API, never an external gateway or a production backdoor.
	cmd.Env = append(os.Environ(), "POLARIS_PORTAL_TEST_URL="+srv.URL, "POLARIS_PORTAL_TEST_COOKIE="+token, "POLARIS_SERVICES_ADMIN_API="+admin.base, "POLARIS_SERVICES_ADMIN_TOKEN="+admin.tok)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, out)
	}
	t.Log(string(out))
}
