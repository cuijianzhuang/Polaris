package agent

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestFetchGeoAssets(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "dat:"+r.URL.Path) }))
	defer srv.Close()
	orig := http.DefaultClient
	http.DefaultClient = srv.Client()
	defer func() { http.DefaultClient = orig }()
	a := &Agent{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "geoip.dat"), []byte("old"), 0o644)
	assets := []model.GeoAsset{{URL: srv.URL + "/ip", File: "geoip.dat"}, {URL: srv.URL + "/ir", File: "geosite_IR.dat"},
		{URL: srv.URL + "/x", File: "../evil.dat"}, {URL: "http://plain/x", File: "plain.dat"}}
	out := a.fetchGeoAssets(t.Context(), dir, assets, false)
	if b, _ := os.ReadFile(filepath.Join(dir, "geoip.dat")); string(b) != "old" {
		t.Fatal("existing file replaced without force")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "geosite_IR.dat")); string(b) != "dat:/ir" {
		t.Fatalf("missing file not downloaded: %v", out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.dat")); err == nil {
		t.Fatal("path traversal")
	}
	a.fetchGeoAssets(t.Context(), dir, assets[:1], true)
	if b, _ := os.ReadFile(filepath.Join(dir, "geoip.dat")); string(b) != "dat:/ip" {
		t.Fatal("force did not refresh")
	}
}
