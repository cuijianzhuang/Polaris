package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// seed fills in only the defaults that are missing; values an admin (or
// an earlier start) already set are never overwritten.
func TestSeedKeepsExistingAndFillsMissing(t *testing.T) {
	s := newTestServer(t) // New already ran seed once
	if err := s.st.SetSetting("site_title", "Custom Title"); err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Where("key = ?", "sub_force_udp").Delete(&store.Setting{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.seed(); err != nil {
		t.Fatal(err)
	}
	get := func(key string) (string, bool) {
		var st store.Setting
		if err := s.st.DB.First(&st, "key = ?", key).Error; err != nil {
			return "", false
		}
		return st.Value, true
	}
	if v, ok := get("site_title"); !ok || v != "Custom Title" {
		t.Fatalf("site_title = %q (found %v), want the existing value kept", v, ok)
	}
	if v, ok := get("sub_force_udp"); !ok || v != "false" {
		t.Fatalf("sub_force_udp = %q (found %v), want the missing default seeded", v, ok)
	}
}
