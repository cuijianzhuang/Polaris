package store

import (
	"path/filepath"
	"testing"
)

func TestSettingCache(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if v := st.GetSetting("k", "def"); v != "def" {
		t.Fatalf("missing setting = %q", v)
	}
	st.SetSetting("k", "v1") // a cached miss is replaced by the write
	if v := st.GetSetting("k", "def"); v != "v1" {
		t.Fatalf("after set = %q", v)
	}
	// Reads come from memory: a write behind the store's back is not seen...
	st.DB.Model(&Setting{}).Where("key = ?", "k").Update("value", "v2")
	if v := st.GetSetting("k", "def"); v != "v1" {
		t.Fatalf("cached read = %q", v)
	}
	// ...until the entry expires.
	st.setMu.Lock()
	c := st.settings["k"]
	c.at = c.at.Add(-2 * settingTTL)
	st.settings["k"] = c
	st.setMu.Unlock()
	if v := st.GetSetting("k", "def"); v != "v2" {
		t.Fatalf("after expiry = %q", v)
	}
	// An empty value is a value, not a miss.
	st.SetSetting("e", "")
	if v := st.GetSetting("e", "def"); v != "" {
		t.Fatalf("empty setting = %q", v)
	}
}
