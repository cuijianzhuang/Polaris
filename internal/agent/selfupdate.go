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
// the supervised worker.
var ErrRestart = errors.New("agent upgrade staged; restart required")

// selfUpdate downloads the agent binary for this architecture from the
// panel's agent endpoint (same mTLS identity as the control stream),
// verifies its SHA-256 and that it runs, then stages a local startup trial.
// The supervisor promotes it only after authenticated configuration application.
func (a *Agent) selfUpdate(ctx context.Context, args map[string]string) (_ string, err error) {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
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
	if !a.cfg.Supervised || a.cfg.UpdateExecutable == "" {
		return "", errors.New("safe upgrade supervisor unavailable; restart the Agent service first")
	}
	if _, statErr := os.Stat(trialPath(a.cfg.DataDir)); !errors.Is(statErr, os.ErrNotExist) {
		return "", errors.New("another agent upgrade is pending")
	}
	exe := a.cfg.UpdateExecutable
	end := a.progress.begin(0, "下载 agent "+version)
	defer func() { end(err) }()
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
	staged := false
	defer func() {
		if !staged {
			_ = os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	body := &progressReader{r: resp.Body, total: max(resp.ContentLength, 0), stage: "下载 agent " + version, p: &a.progress}
	_, err = io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, 200<<20))
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("checksum mismatch (want %s, got %s)", want, got)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	a.progress.step("校验 agent "+version, 0, 0)
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
	if got, err := fileDigest(exe); err == nil && got == want {
		return "already " + newVersion, nil
	}
	if err := syncDir(filepath.Dir(exe)); err != nil {
		return "", err
	}
	if err := stageUpgrade(a.cfg.DataDir, exe, tmp.Name(), want, newVersion); err != nil {
		return "", err
	}
	staged = true
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
