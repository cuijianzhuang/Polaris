package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/pki"
)

// UpgradeRestartExit is private to the worker/supervisor protocol. The stable
// executable is not replaced until the candidate has authenticated and applied
// the panel's current configuration. A stopped/dead worker cannot confirm itself.
const UpgradeRestartExit = 75

type upgradeTrial struct {
	Format     int    `json:"format"`
	Executable string `json:"executable"`
	Candidate  string `json:"candidate"`
	SHA256     string `json:"sha256"`
	Version    string `json:"version"`
	Nonce      string `json:"nonce"`
}

func trialPath(data string) string { return filepath.Join(data, "agent", "upgrade-trial.json") }
func readyPath(data string) string { return filepath.Join(data, "agent", "upgrade-ready") }

func durableWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".upgrade-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err == nil {
		err = syncDir(filepath.Dir(path))
	}
	return err
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readTrial(data, executable string) (*upgradeTrial, error) {
	b, err := os.ReadFile(trialPath(data))
	if err != nil {
		return nil, err
	}
	var t upgradeTrial
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	nonce, nonceErr := base64.RawURLEncoding.DecodeString(t.Nonce)
	digest, err := hex.DecodeString(t.SHA256)
	if err != nil || len(digest) != sha256.Size || t.Format != 1 || nonceErr != nil || len(nonce) != 32 || t.Version == "" ||
		t.Executable != executable || filepath.Dir(t.Candidate) != filepath.Dir(executable) ||
		!strings.HasPrefix(filepath.Base(t.Candidate), ".polaris-agent-new-") {
		return nil, errors.New("invalid agent upgrade journal")
	}
	return &t, nil
}

func (t *upgradeTrial) ready(data string) bool {
	b, err := os.ReadFile(readyPath(data))
	return err == nil && string(b) == t.Nonce
}

func clearTrial(data string, t *upgradeTrial) error {
	if t != nil {
		if err := os.Remove(t.Candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Remove and sync the journal BEFORE its confirmation. A power loss after
	// promotion must never leave a journal with its proof already deleted.
	if err := os.Remove(trialPath(data)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDir(filepath.Join(data, "agent")); err != nil {
		return err
	}
	if err := os.Remove(readyPath(data)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Join(data, "agent"))
}

// RecoverUpgrade never resumes an unconfirmed trial after a supervisor/host
// restart. The entrypoint is still the previous binary. If a power loss cut off
// cleanup after promotion, the persisted nonce and digest prove it was confirmed.
func RecoverUpgrade(data, executable string) error {
	t, err := readTrial(data, executable)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	} // fail closed; never execute a path from a bad journal
	got, err := fileDigest(executable)
	if err != nil {
		return err
	}
	if got == t.SHA256 && !t.ready(data) {
		return errors.New("promoted agent has no startup confirmation")
	}
	if got != t.SHA256 {
		_ = recordUpgradeResult(data, t, "rolled_back", "supervisor restarted before startup confirmation")
	}
	return clearTrial(data, t)
}

func stageUpgrade(data, executable, candidate, digest, version string) error {
	if _, err := os.Stat(trialPath(data)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("another agent upgrade is pending")
	}
	t := upgradeTrial{Format: 1, Executable: executable, Candidate: candidate, SHA256: digest, Version: version, Nonce: pki.Token(32)}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_ = os.Remove(readyPath(data))
	return durableWrite(trialPath(data), b, 0o600)
}

func promoteUpgrade(data string, t *upgradeTrial) (committed bool, err error) {
	if !t.ready(data) {
		return false, errors.New("candidate is not confirmed")
	}
	got, err := fileDigest(t.Candidate)
	if err != nil || got != t.SHA256 {
		return false, errors.New("candidate checksum changed before promotion")
	}
	// Hardlink the old inode before replacing the path. The backup is executable
	// and retained for manual recovery; the running supervisor also stays old.
	backup := t.Executable + ".previous-" + t.Nonce
	if err := os.Link(t.Executable, backup); err != nil {
		return false, fmt.Errorf("retain previous agent: %w", err)
	}
	defer os.Remove(backup)
	if err := os.Rename(backup, t.Executable+".previous"); err != nil {
		return false, err
	}
	if err := syncDir(filepath.Dir(t.Executable)); err != nil {
		return false, err
	}
	if err := os.Rename(t.Candidate, t.Executable); err != nil {
		return false, err
	}
	if err := syncDir(filepath.Dir(t.Executable)); err != nil {
		return true, err
	}
	return true, clearTrial(data, t)
}

// Supervise runs as the service's main process. Workers are parent-bound, so
// systemd does not kill the rollback supervisor when a candidate crashes. The
// same worker protocol is retained across releases; never exec over the parent.
func Supervise(ctx context.Context, executable, data string, args []string, log *slog.Logger) error {
	return supervise(ctx, executable, data, args, log, 5*time.Minute)
}

func supervise(ctx context.Context, executable, data string, args []string, log *slog.Logger, timeout time.Duration) error {
	if err := RecoverUpgrade(data, executable); err != nil {
		return err
	}
	var trial *upgradeTrial
	path := executable
	for ctx.Err() == nil {
		p := core.NewProcess("agent-worker", path, "", log, args...)
		p.BindToParent = core.ParentBoundSupported()
		p.Filter = func(line string) bool { fmt.Fprintln(os.Stderr, line); return true }
		err := p.Start(ctx)
		if err != nil && p.LastError() != nil {
			err = p.LastError()
		}
		deadline := time.Now().Add(timeout)
		for err == nil && p.Running() && ctx.Err() == nil {
			if trial != nil {
				if trial.ready(data) {
					committed, promoteErr := promoteUpgrade(data, trial)
					err = promoteErr
					if committed {
						log.Info("agent upgrade confirmed", "version", trial.Version)
						if recordErr := recordUpgradeResult(data, trial, "confirmed", ""); recordErr != nil {
							log.Warn("record agent upgrade", "err", recordErr)
						}
						trial, path = nil, executable
					}
				} else if time.Now().After(deadline) {
					err = errors.New("agent startup confirmation timed out")
				}
			}
			if err != nil {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = p.Stop(stopCtx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = p.LastError()
		}
		if trial != nil {
			log.Error("agent candidate failed; returning to previous binary", "err", err)
			if recordErr := recordUpgradeResult(data, trial, "rolled_back", fmt.Sprint(err)); recordErr != nil {
				log.Warn("record rollback result", "err", recordErr)
			}
			if clearErr := clearTrial(data, trial); clearErr != nil {
				return clearErr
			}
			trial, path = nil, executable
			continue
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != UpgradeRestartExit {
			return err
		}
		trial, err = readTrial(data, executable)
		if err != nil {
			return fmt.Errorf("upgrade restart without valid journal: %w", err)
		}
		got, err := fileDigest(trial.Candidate)
		if err != nil || got != trial.SHA256 {
			_ = recordUpgradeResult(data, trial, "rolled_back", "candidate checksum changed or file is unavailable")
			if err = clearTrial(data, trial); err != nil {
				return err
			}
			trial, path = nil, executable
			continue
		}
		path = trial.Candidate
	}
	return nil
}

func (a *Agent) confirmUpgrade() {
	if !a.cfg.Supervised || a.appliedRev() == 0 {
		return
	}
	t, err := readTrial(a.cfg.DataDir, a.cfg.UpdateExecutable)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		a.log.Error("read upgrade confirmation", "err", err)
		return
	}
	exe, err := os.Executable()
	if err != nil || exe != t.Candidate || t.Version != Version {
		return
	}
	for _, m := range a.coreSnapshot() {
		if !m.adapter.Health(context.Background()).Running {
			return
		}
	}
	if err := a.sendHeartbeat(); err != nil {
		return
	}
	if !t.ready(a.cfg.DataDir) {
		if err := durableWrite(readyPath(a.cfg.DataDir), []byte(t.Nonce), 0o600); err != nil {
			a.log.Error("confirm agent upgrade", "err", err)
		}
	}
}

func (a *Agent) reportedVersion() string {
	// Rollout health must not accept a trial heartbeat before local promotion.
	if a.upgradePending() {
		return Version + "-trial"
	}
	return Version
}

func (a *Agent) upgradePending() bool {
	if !a.cfg.Supervised {
		return false
	}
	_, err := os.Stat(trialPath(a.cfg.DataDir))
	return !errors.Is(err, os.ErrNotExist)
}

type upgradeResult struct {
	Version string    `json:"version"`
	State   string    `json:"state"`
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

func recordUpgradeResult(data string, t *upgradeTrial, state, message string) error {
	b, err := json.Marshal(upgradeResult{Version: t.Version, State: state, Error: message, At: time.Now().UTC()})
	if err != nil {
		return err
	}
	return durableWrite(filepath.Join(data, "agent", "upgrade-result.json"), b, 0o600)
}

func (a *Agent) reportUpgradeResult() {
	if a.upgradePending() {
		return
	}
	b, err := os.ReadFile(filepath.Join(a.cfg.DataDir, "agent", "upgrade-result.json"))
	if err != nil {
		return
	}
	var r upgradeResult
	if json.Unmarshal(b, &r) != nil || r.State != "rolled_back" {
		return
	}
	a.progress.emit(&polarisv1.Progress{Stage: "Agent 升级已回退：" + r.Version, Error: r.Error, StartedAt: r.At.Unix()})
}
