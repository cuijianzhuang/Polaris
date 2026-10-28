package store

import (
	"path/filepath"
	"testing"
)

// TestSelfServiceBackfill: on upgrade, accounts the shop created before the
// mark existed get it, recognised by their remark; others do not.
func TestSelfServiceBackfill(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.db")
	st, err := Open(p, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	st.DB.Create(&User{ID: "a", Username: "tg1", UUID: "a", SubToken: "a", Remark: "Telegram 购买 Bob"})
	st.DB.Create(&User{ID: "b", Username: "tgwabc", UUID: "b", SubToken: "b", Remark: "网页购买 o1"})
	st.DB.Create(&User{ID: "c", Username: "vip", UUID: "c", SubToken: "c", Remark: "老客户"})
	// A database from before the mark (and before versioned migrations).
	if err := st.DB.Migrator().DropColumn(&User{}, "self_service"); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Migrator().DropTable(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	db, _ := st.DB.DB()
	db.Close()
	st, err = Open(p, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	st.DB.Model(&User{}).Where("self_service = ?", true).Order("id").Pluck("id", &ids)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("marked %v", ids)
	}
	// Later starts leave the mark to the admin.
	st.DB.Model(&User{}).Where("id = ?", "a").Update("self_service", false)
	db, _ = st.DB.DB()
	db.Close()
	st, _ = Open(p, nil, false)
	var n int64
	st.DB.Model(&User{}).Where("self_service = ?", true).Count(&n)
	if n != 1 {
		t.Fatalf("backfill ran again: %d", n)
	}
}
