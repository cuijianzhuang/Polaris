package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallScriptPinsAgentChecksum(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.Router())
	defer srv.Close()
	bin := []byte("agent-binary")
	os.MkdirAll(filepath.Join(s.cfg.DataDir, "downloads"), 0o755)
	os.WriteFile(filepath.Join(s.cfg.DataDir, "downloads", "polaris-agent-linux-amd64"), bin, 0o755)
	sum := sha256.Sum256(bin)

	resp, err := http.Get(srv.URL + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	script := string(b)
	for _, want := range []string{`SHA_amd64="` + hex.EncodeToString(sum[:]) + `"`, `SHA_arm64=""`, `PANEL_VERSION="dev"`, "checksum mismatch"} {
		if !strings.Contains(script, want) {
			t.Fatalf("install.sh missing %q", want)
		}
	}
	if strings.Contains(script, "__") {
		t.Fatal("unreplaced placeholder in install.sh")
	}
}
