package store

import (
	"encoding/json"
	"github.com/cuijianzhuang/polaris/internal/secret"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacySubscriptionSecretsEncryptedOnOpen(t *testing.T) {
	box, _ := secret.New("storage-key")
	path := filepath.Join(t.TempDir(), "old.db")
	st, e := Open(path, box, false)
	if e != nil {
		t.Fatal(e)
	}
	old := SubSource{ID: "legacy", Name: "Legacy", URL: "https://user:password@provider.example/sub?token=secret-token", Content: "trojan://secret-password@node.example:443", Cache: `[{"Password":"cache-secret"}]`}
	if e = st.DB.Create(&old).Error; e != nil {
		t.Fatal(e)
	}
	db, _ := st.DB.DB()
	db.Close()
	st, e = Open(path, box, false)
	if e != nil {
		t.Fatal(e)
	}
	db, _ = st.DB.DB()
	defer db.Close()
	var row SubSource
	if e = st.DB.First(&row).Error; e != nil {
		t.Fatal(e)
	}
	for _, v := range []string{row.URL, row.Content, row.Cache} {
		if !strings.HasPrefix(v, "enc:v1:") {
			t.Fatal("legacy plaintext", v)
		}
	}
	if box.MustOpen(row.URL) != old.URL || box.MustOpen(row.Content) != old.Content || box.MustOpen(row.Cache) != old.Cache {
		t.Fatal("migration changed data")
	}
	b, _ := json.Marshal(row)
	if strings.Contains(string(b), "enc:v1:") || strings.Contains(string(b), "secret-") || strings.Contains(string(b), "url\"") {
		t.Fatal("sensitive columns escaped through API", string(b))
	}
}
