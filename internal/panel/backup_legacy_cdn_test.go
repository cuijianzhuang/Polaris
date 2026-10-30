package panel

import (
	"bytes"
	"encoding/gob"
	"path/filepath"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/backupfmt"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestRestoreLegacyCDNPinsSurviveRefresh(t *testing.T)   { testRestoreCDNPins(t, true) }
func TestRestoreModernEmptyCDNPinsRemainEmpty(t *testing.T) { testRestoreCDNPins(t, false) }

func TestRestoreHTTPClientSchemeSurvivesRefresh(t *testing.T) { testRestoreCDNPins(t, false, "http") }

func testRestoreCDNPins(t *testing.T, legacy bool, schemes ...string) {
	scheme := "https"
	if len(schemes) > 0 {
		scheme = schemes[0]
	}
	s := newTestServer(t)
	if err := s.st.DB.Create(&store.CDNProfile{Scheme: scheme, ID: "legacy", Name: "legacy", Provider: "cloudflare", PreferredIPs: store.NewJSON([]string{"104.16.1.1", "preferred.example.com"})}).Error; err != nil {
		t.Fatal(err)
	}
	files, _, err := backupfmt.ExportAll(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err = gob.NewDecoder(bytes.NewReader(files["cdn_profiles"])).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if legacy {
			delete(r, "pinned_hosts")
			delete(r, "scheme")
		} else {
			r["pinned_hosts"] = "[]"
		}
	}
	var b bytes.Buffer
	if err = gob.NewEncoder(&b).Encode(rows); err != nil {
		t.Fatal(err)
	}
	files["cdn_profiles"] = b.Bytes()
	dir := t.TempDir()
	path := filepath.Join(dir, "restored.db")
	dst, err := store.Open(path, s.st.Box, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = restoreArchive(dst, dir, &backupfmt.Archive{Tables: files, Manifest: backupfmt.Manifest{Format: 2}}, s.st.Box); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := dst.DB.DB()
	sqlDB.Close()
	dst, err = store.Open(path, s.st.Box, false)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ = dst.DB.DB()
	defer sqlDB.Close()
	var p store.CDNProfile
	if err = dst.DB.First(&p, "id = ?", "legacy").Error; err != nil {
		t.Fatal(err)
	}
	if p.Scheme != scheme {
		t.Fatalf("restored scheme = %q, want %q", p.Scheme, scheme)
	}
	if (legacy && (len(p.PinnedHosts.V) != 1 || p.PinnedHosts.V[0] != "preferred.example.com")) || (!legacy && len(p.PinnedHosts.V) != 0) {
		t.Fatal("legacy domain not pinned", p.PinnedHosts.V)
	}
	dst.DB.Create(&store.CDNIPReport{Scheme: cdnScheme(&p), Port: cdnDefaultPort(&p), ProfileID: p.ID, Source: "review", At: time.Now(), IPs: store.NewJSON([]store.CDNIPResult{{IP: "104.16.2.2", LatencyMs: 10}})})
	if err = dst.DB.Transaction(func(tx *gorm.DB) error { _, _, e := rebuildCDNLists(tx, &p, time.Now(), nil); return e }); err != nil {
		t.Fatal(err)
	}
	if err = dst.DB.First(&p, "id = ?", "legacy").Error; err != nil {
		t.Fatal(err)
	}
	if (legacy && (len(p.PreferredIPs.V) != 2 || p.PreferredIPs.V[1] != "preferred.example.com")) || (!legacy && len(p.PreferredIPs.V) != 1) {
		t.Fatal("refresh dropped restored pinned host", p.PreferredIPs.V)
	}
}
