package panel

import (
	"fmt"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestUSDTSettings: USDT addresses, networks and RPC endpoints are checked
// before they are saved.
func TestUSDTSettings(t *testing.T) {
	_, _, admin := newAdminClient(t)
	for _, bad := range []map[string]any{
		{"shop_usdt_address": "0x1234"},
		{"shop_usdt_evm_address": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"},
		{"shop_usdt_chains": "tron,eth"},
		{"shop_usdt_rpc_bsc": "ftp://x"},
	} {
		if code, _ := admin.do("PUT", "/settings", bad); code != 400 {
			t.Fatalf("saved %v: %d", bad, code)
		}
	}
	ok := map[string]any{"shop_usdt_address": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "shop_usdt_evm_address": "0x55d398326f99059fF775485246999027B3197955",
		"shop_usdt_chains": "tron,bsc,polygon", "shop_usdt_rpc_bsc": "https://bsc.example.com"}
	if code, out := admin.do("PUT", "/settings", ok); code != 200 {
		t.Fatalf("save %d %v", code, out)
	}
}

// TestUSDTAmountsPerNetwork: unique amounts are kept per network; orders
// from before networks existed count as TRON.
func TestUSDTAmountsPerNetwork(t *testing.T) {
	s, _, _ := newAdminClient(t)
	now := time.Now()
	s.st.SetSetting("shop_usdt_address", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	s.st.SetSetting("shop_usdt_evm_address", "0x55d398326f99059fF775485246999027B3197955")
	quote := func(chain string) (string, error) {
		o, e := s.reserveUSDTOrder(&store.Order{ID: store.NewID(), Chain: chain, Method: payUSDT, Status: "pending", CreatedAt: now}, 1, now)
		if e != nil {
			return "", e
		}
		return o.Amount, nil
	}
	for i := 1; i <= 500; i++ {
		a := float64(1e4+i) / 1e4
		s.st.DB.Create(&store.Order{ID: "old" + store.NewID(), Method: payUSDT, Status: "pending", Amount: fmt.Sprintf("%.4f", a), CreatedAt: now})
	}
	if _, err := quote("tron"); err == nil {
		t.Fatal("TRON amounts not taken by legacy orders")
	}
	if a, err := quote("bsc"); err != nil || a == "" {
		t.Fatalf("BSC amount blocked by TRON orders: %q %v", a, err)
	}
	if n, ok := usdtNetByID(""); !ok || n.ID != "tron" {
		t.Fatal("legacy orders are not TRON")
	}
	if _, ok := usdtNetByID("eth"); ok {
		t.Fatal("unknown network")
	}
}
