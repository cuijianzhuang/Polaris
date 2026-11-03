package panel

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLiveWebsitesBrowser(t *testing.T) {
	if os.Getenv("POLARIS_WEBSITES_BROWSER_TEST") == "" {
		t.Skip("POLARIS_WEBSITES_BROWSER_TEST required")
	}
	s, a, _ := websiteFixture(t)
	dir, err := filepath.Abs("../../web/dist")
	if err != nil {
		t.Fatal(err)
	}
	s.webFS = http.Dir(dir)
	srv := httptest.NewServer(s.Router())
	defer srv.Close()
	s.cfg.PublicURL = srv.URL
	bundle := filepath.Join(t.TempDir(), "blog.zip")
	file, err := os.Create(bundle)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(file)
	for name, data := range map[string]string{"index.html": "<h1>My blog</h1>", "assets/site.css": "body{color:red}"} {
		w, _ := z.Create(name)
		w.Write([]byte(data))
	}
	z.Close()
	file.Close()
	cmd := exec.CommandContext(t.Context(), "node", "../../web/scripts/websites-live.mjs")
	cmd.Env = append(os.Environ(), "POLARIS_WEBSITES_TEST_URL="+srv.URL, "POLARIS_WEBSITES_TEST_TOKEN="+a.tok, "POLARIS_WEBSITES_TEST_ZIP="+bundle)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, out)
	}
	t.Log(string(out))
}
