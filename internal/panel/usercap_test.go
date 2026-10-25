package panel

import (
	"errors"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A shop order or redeem code that would create an account beyond the
// licence's user limit is refused up front, before anything is paid.
func TestShopUserCap(t *testing.T) {
	s, _, _ := newAdminClient(t)
	s.st.SetSetting("shop_enabled", "true")
	s.st.SetSetting("shop_auto_create", "true")
	s.st.SetSetting("shop_manual", "true")
	s.st.DB.Create(&store.Group{ID: "p1", Name: "月付", Plan: true, Sale: true, PriceCNY: 15, PriceStars: 50})
	s.st.DB.Create(&store.User{ID: "u1", Username: "a", UUID: "x", Password: "p", Status: "active", SubToken: "aaaaaaaaaaaaaaaaaaaaaa"})
	s.lic.Store(&licenseState{st: license.State{Plan: "pro", Limits: license.Limits{Users: 1}}})
	if _, err := s.newOrderFor(shopBuyer{TGID: 9}, "p1", payStars, ""); !errors.Is(err, errUserCap) {
		t.Fatalf("new buyer over the cap: %v", err)
	}
	if _, _, err := s.redeemFor(shopBuyer{TGID: 9}, "ANYCODE"); !errors.Is(err, errUserCap) {
		t.Fatalf("redeem over the cap: %v", err)
	}
	// An existing account can still buy.
	if _, err := s.newOrderFor(shopBuyer{TGID: 9, UserID: "u1"}, "p1", payStars, ""); errors.Is(err, errUserCap) {
		t.Fatal("existing account refused")
	}
}
