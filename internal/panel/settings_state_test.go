package panel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestSettingsEffectiveSwitchDefaultsAndExplicitValues(t *testing.T) {
	s, _, client := newAdminClient(t)
	// Migrations seed some values; exercise actual absent-row behavior.
	for key := range switchSettingDefaults {
		if err := s.st.DB.Delete(&store.Setting{}, "key = ?", key).Error; err != nil {
			t.Fatal(err)
		}
		s.st.ForgetSetting(key)
	}
	code, out := client.do("GET", "/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("settings read: %d", code)
	}
	for key, want := range map[string]string{"tg_enabled": "false", "tg_notify_users": "true", "tg_daily_report": "true", "probe_enabled": "true", "sub_show_bot": "true", "shop_auto_create": "true", "mail_notify_users": "true", "mail_notify_admin": "true"} {
		if out[key] != want {
			t.Errorf("%s: got %v, want %s", key, out[key], want)
		}
		var count int64
		s.st.DB.Model(&store.Setting{}).Where("key = ?", key).Count(&count)
		if count != 0 {
			t.Fatalf("read persisted missing default %s", key)
		}
	}
	values := map[string]string{"tg_enabled": "true", "tg_notify_users": "false", "tg_daily_report": "", "mail_notify_users": "", "sub_show_bot": "false", "sub_host_strict": "0", "tg_daily_report_hour": "0"}
	if err := s.st.SetSettings(values); err != nil {
		t.Fatal(err)
	}
	s.st.SetSetting("tg_token", s.st.Box.Seal("settings-state-secret"))
	_, out = client.do("GET", "/settings", nil)
	for key, want := range values {
		if out[key] != want {
			t.Errorf("explicit %s: got %v, want %q", key, out[key], want)
		}
	}
	if out["tg_token"] != nil || out["tg_token_set"] != true {
		t.Fatal("token must remain redacted")
	}
	_, status := client.do("GET", "/telegram", nil)
	if status["running"] != false {
		t.Fatalf("runtime unexpectedly follows enabled configuration: %v", status)
	}
}

func TestSettingsReadFailureIsNotAnEmptyConfiguration(t *testing.T) {
	s := newTestServer(t)
	const callback = "test_settings_read_failure"
	if err := s.st.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "settings" {
			tx.AddError(errors.New("synthetic settings read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Query().Remove(callback) })
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("GET", "/settings", nil)
	s.getSettings(context)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("failed read returned %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestSettingsInvalidBatchDoesNotChangeSwitches(t *testing.T) {
	s, _, client := newAdminClient(t)
	s.st.SetSetting("probe_enabled", "true")
	s.st.SetSetting("site_title", "before")
	// Repeat because Go map iteration deliberately has no field order.
	for i := 0; i < 8; i++ {
		code, _ := client.do("PUT", "/settings", map[string]string{"probe_enabled": "false", "site_title": "after", "sub_update_interval": "not-a-number"})
		if code != http.StatusBadRequest {
			t.Fatalf("invalid settings: %d", code)
		}
		if s.st.GetSetting("probe_enabled", "") != "true" || s.st.GetSetting("site_title", "") != "before" {
			t.Fatal("a rejected settings batch partially committed")
		}
	}
}

func TestSettingsStorageFailureRollsBackAllSwitches(t *testing.T) {
	s, _, client := newAdminClient(t)
	s.st.SetSetting("probe_enabled", "true")
	s.st.SetSetting("site_title", "before")
	const callback = "test_settings_write_failure"
	if err := s.st.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if setting, ok := tx.Statement.Dest.(*store.Setting); ok && setting.Key == "site_title" {
			tx.AddError(errors.New("synthetic settings write failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Update().Remove(callback) })
	code, _ := client.do("PUT", "/settings", map[string]string{"probe_enabled": "false", "site_title": "after"})
	if code != http.StatusInternalServerError {
		t.Fatalf("storage failure returned %d", code)
	}
	s.st.ForgetSetting("probe_enabled")
	s.st.ForgetSetting("site_title")
	if s.st.GetSetting("probe_enabled", "") != "true" || s.st.GetSetting("site_title", "") != "before" {
		t.Fatal("a failed transaction partially committed")
	}
}
