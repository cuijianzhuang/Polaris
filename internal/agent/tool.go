package agent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// panelEnv marks a machine that runs the panel; its polaris tool belongs to
// the panel install and is left alone.
const panelEnv = "/etc/polaris/polaris.env"

// syncTool installs or refreshes the `polaris` management script from the
// panel over the agent endpoint, so nodes installed before the script was
// shipped (or upgraded only from the web UI) get it too. Best effort.
func (a *Agent) syncTool(ctx context.Context) {
	path := a.cfg.ToolPath
	if path == "" || runtime.GOOS != "linux" {
		return
	}
	if _, err := os.Stat(panelEnv); err == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.cfg.PanelURL, "/")+"/agent/tool", nil)
	if err != nil {
		return
	}
	resp, err := httpClient(a.id.tlsConfig()).Do(req)
	if err != nil {
		a.log.Debug("fetch polaris tool", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		a.log.Debug("fetch polaris tool", "status", resp.Status)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil || !bytes.HasPrefix(body, []byte("#!")) || !bytes.Contains(body, []byte("polaris")) {
		return
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, body) {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".polaris-new-*")
	if err != nil {
		a.log.Warn("install polaris tool", "err", err)
		return
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	_, err = tmp.Write(body)
	tmp.Close()
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o755)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		a.log.Warn("install polaris tool", "err", err)
		return
	}
	if ctl := filepath.Join(dir, "polaris-ctl"); func() bool { _, err := os.Lstat(ctl); return err != nil }() {
		_ = os.Symlink(path, ctl)
	}
	a.log.Info("polaris management tool updated", "path", path)
}
