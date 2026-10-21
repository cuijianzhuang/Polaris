package store

import (
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
)

func TestSetSettingsCommitsBeforeInvalidation(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "settings.db"), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, key := range []string{"a", "z"} {
		if err := st.SetSetting(key, "old"); err != nil {
			t.Fatal(err)
		}
		if st.GetSetting(key, "") != "old" {
			t.Fatal("old cache not loaded")
		}
	}
	var notified []string
	st.OnSettingChange = func(key string) {
		var rows []Setting
		if err := st.DB.Order("key").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 || rows[0].Value != "new" || rows[1].Value != "new" {
			t.Fatal("notification preceded group commit", rows)
		}
		if st.GetSetting(key, "") != "new" {
			t.Fatal("cache kept old group value")
		}
		notified = append(notified, key)
	}
	st.DB.Callback().Update().Before("gorm:update").Register("test:settings-fail", func(tx *gorm.DB) {
		if setting, ok := tx.Statement.Dest.(*Setting); ok && setting.Key == "z" {
			tx.AddError(errors.New("second setting failed"))
		}
	})
	if err := st.SetSettings(map[string]string{"a": "new", "z": "new"}); err == nil {
		t.Fatal("partial settings write succeeded")
	}
	st.DB.Callback().Update().Remove("test:settings-fail")
	if len(notified) != 0 {
		t.Fatal("failed write published invalidation")
	}
	var rows []Setting
	st.DB.Order("key").Find(&rows)
	if len(rows) != 2 || rows[0].Value != "old" || rows[1].Value != "old" || st.GetSetting("a", "") != "old" || st.GetSetting("z", "") != "old" {
		t.Fatal("failed write partially changed settings", rows)
	}
	if err := st.SetSettings(map[string]string{"a": "new", "z": "new"}); err != nil {
		t.Fatal(err)
	}
	if len(notified) != 2 || notified[0] != "a" || notified[1] != "z" {
		t.Fatal("missing settings invalidations", notified)
	}
}
