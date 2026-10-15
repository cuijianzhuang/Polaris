package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrRestart is returned by Run after a self-update: the caller re-executes
// the (new) binary in place.
var ErrRestart = errors.New("agent binary replaced; restart required")

// selfUpdate downloads the agent binary for this architecture from the
// panel's agent endpoint (same mTLS identity as the control stream),
// verifies its SHA-256 and that it runs, and atomically replaces the running
// executable. It does not restart; see requestRestart.
func (a *Agent) selfUpdate(ctx context.Context, args map[string]string) (string, error) {
	want := strings.ToLower(args["sha256_"+runtime.GOARCH])
	version := args["version"]
	if version != "" && version == Version && version != "dev" {
		return "already " + Version, nil
	}
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("self-update is only supported on linux (this is %s)", runtime.GOOS)
	}
	if want == "" {
		return "", fmt.Errorf("the panel has no agent binary for %s", runtime.GOARCH)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.cfg.PanelURL, "/")+"/agent/binary/"+runtime.GOARCH, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient(a.id.tlsConfig()).Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: panel returned %s", resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".polaris-agent-new-*")
	if err != nil {
		return "", fmt.Errorf("cannot write next to %s: %w", exe, err)
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, 200<<20))
	tmp.Close()
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("checksum mismatch (want %s, got %s)", want, got)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	// The new binary must at least start on this machine.
	vctx, vcancel := context.WithTimeout(ctx, 10*time.Second)
	defer vcancel()
	out, err := exec.CommandContext(vctx, tmp.Name(), "--version").Output()
	if err != nil {
		return "", fmt.Errorf("new binary does not run: %w", err)
	}
	newVersion := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "polaris-agent"))
	if version != "" && newVersion != version {
		return "", fmt.Errorf("downloaded agent reports version %q, expected %q", newVersion, version)
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return "", fmt.Errorf("replace %s: %w", exe, err)
	}
	return fmt.Sprintf("%s → %s", Version, newVersion), nil
}

// requestRestart makes Run stop the cores and return ErrRestart.
func (a *Agent) requestRestart() {
	a.restartMu.Lock()
	a.restart = true
	cancel := a.cancel
	a.restartMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
