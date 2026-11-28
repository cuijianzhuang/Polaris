package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

// Real child processes exercise the worker protocol, including crashes and a
// SIGSTOP'ed candidate that cannot run its own timeout/cleanup code.
func TestUpgradeWorkerHelper(t *testing.T) {
	if os.Getenv("POLARIS_UPGRADE_TEST") != "1" {
		return
	}
	data, entry, candidate := os.Getenv("UPGRADE_DATA"), os.Getenv("UPGRADE_ENTRY"), os.Getenv("UPGRADE_CANDIDATE")
	exe, _ := os.Executable()
	if exe == entry {
		if _, err := os.Stat(filepath.Join(data, "attempted")); errors.Is(err, os.ErrNotExist) {
			digest, err := fileDigest(candidate)
			if err != nil {
				os.Exit(2)
			}
			if os.Getenv("UPGRADE_CASE") == "tampered" {
				digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}
			if stageUpgrade(data, entry, candidate, digest, "test-new", nil) != nil {
				os.Exit(3)
			}
			_ = os.WriteFile(filepath.Join(data, "attempted"), nil, 0o600)
			os.Exit(UpgradeRestartExit)
		}
		_ = os.WriteFile(filepath.Join(data, "fallback"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	} else {
		_ = os.WriteFile(filepath.Join(data, "candidate-pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		switch os.Getenv("UPGRADE_CASE") {
		case "crash":
			os.Exit(2)
		case "stopped":
			_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
		case "confirmed":
			trial, err := readTrial(data, entry)
			if err != nil || durableWrite(readyPath(data), []byte(trial.Nonce), 0o600) != nil {
				os.Exit(4)
			}
		}
	}
	for {
		time.Sleep(time.Second)
	}
}

func copyUpgradeExecutable(t *testing.T, path string, marker byte) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(out, f)
	if err == nil {
		_, err = out.Write([]byte{marker})
	} // distinct digests, same ELF program
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeSupervisorRollbackAndPromotion(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux child lifetime guarantee")
	}
	for _, kind := range []string{"crash", "stopped", "unconfirmed", "tampered", "confirmed"} {
		t.Run(kind, func(t *testing.T) {
			data := t.TempDir()
			if err := os.Mkdir(filepath.Join(data, "agent"), 0o700); err != nil {
				t.Fatal(err)
			}
			entry, candidate := filepath.Join(data, "agent-bin"), filepath.Join(data, ".polaris-agent-new-test")
			copyUpgradeExecutable(t, entry, 1)
			copyUpgradeExecutable(t, candidate, 2)
			old, _ := fileDigest(entry)
			newDigest, _ := fileDigest(candidate)
			for k, v := range map[string]string{"POLARIS_UPGRADE_TEST": "1", "UPGRADE_DATA": data, "UPGRADE_ENTRY": entry, "UPGRADE_CANDIDATE": candidate, "UPGRADE_CASE": kind} {
				t.Setenv(k, v)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- supervise(ctx, entry, data, []string{"-test.run=^TestUpgradeWorkerHelper$"}, slog.New(slog.NewTextHandler(io.Discard, nil)), 200*time.Millisecond)
			}()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(20 * time.Second):
					t.Error("supervisor did not stop")
				}
			}()
			deadline := time.Now().Add(18 * time.Second)
			for {
				b, _ := os.ReadFile(filepath.Join(data, "agent", "upgrade-result.json"))
				var result upgradeResult
				_ = json.Unmarshal(b, &result)
				want := "rolled_back"
				if kind == "confirmed" {
					want = "confirmed"
				}
				_, fallback := os.Stat(filepath.Join(data, "fallback"))
				if result.State == want && (kind == "confirmed" || fallback == nil) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("no %s result/fallback: %s", want, b)
				}
				time.Sleep(20 * time.Millisecond)
			}
			got, _ := fileDigest(entry)
			want := old
			if kind == "confirmed" {
				want = newDigest
				if backup, _ := fileDigest(entry + ".previous"); backup != old {
					t.Fatal("previous confirmed executable not retained")
				}
			}
			if got != want {
				t.Fatal("wrong stable executable after trial")
			}
			if _, err := os.Stat(trialPath(data)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("trial journal not cleared")
			}
			if kind != "confirmed" {
				if b, err := os.ReadFile(filepath.Join(data, "candidate-pid")); err == nil {
					pid, _ := strconv.Atoi(string(b))
					if syscall.Kill(pid, 0) == nil {
						t.Fatal("candidate is still alive while fallback is running")
					}
				}
			}
		})
	}
}

func TestUpgradeRecoveryAfterRestart(t *testing.T) {
	for _, promoted := range []bool{false, true} {
		t.Run(strconv.FormatBool(promoted), func(t *testing.T) {
			data := t.TempDir()
			_ = os.Mkdir(filepath.Join(data, "agent"), 0o700)
			entry, candidate := filepath.Join(data, "agent-bin"), filepath.Join(data, ".polaris-agent-new-test")
			_ = os.WriteFile(entry, []byte("previous"), 0o755)
			_ = os.WriteFile(candidate, []byte("new"), 0o755)
			digest, _ := fileDigest(candidate)
			if err := stageUpgrade(data, entry, candidate, digest, "new", nil); err != nil {
				t.Fatal(err)
			}
			trial, err := readTrial(data, entry)
			if err != nil {
				t.Fatal(err)
			}
			if promoted {
				if err := durableWrite(readyPath(data), []byte(trial.Nonce), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(candidate, entry); err != nil {
					t.Fatal(err)
				}
			}
			if err := RecoverUpgrade(data, entry); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(entry)
			want := "previous"
			if promoted {
				want = "new"
			}
			if string(b) != want {
				t.Fatal("restart changed executable")
			}
			if err := RecoverUpgrade(data, entry); err != nil {
				t.Fatal("recovery not idempotent", err)
			}
		})
	}
}

func TestUpgradeRejectsInvalidJournalAndUnsupervisedUpdate(t *testing.T) {
	data := t.TempDir()
	_ = os.Mkdir(filepath.Join(data, "agent"), 0o700)
	entry := filepath.Join(data, "agent-bin")
	_ = os.WriteFile(trialPath(data), []byte(`{"format":1,"candidate":"/tmp/another-executable"}`), 0o600)
	if err := RecoverUpgrade(data, entry); err == nil {
		t.Fatal("accepted untrusted journal")
	}
	a := &Agent{}
	if _, err := a.selfUpdate(context.Background(), map[string]string{"sha256_" + runtime.GOARCH: "abc", "version": "new"}); err == nil {
		t.Fatal("allowed upgrade without rollback supervisor")
	}
}

type stubCoreAdapter struct {
	core.CoreAdapter
	running bool
}

func (s stubCoreAdapter) Health(context.Context) model.CoreStatus {
	return model.CoreStatus{Running: s.running}
}

// The confirmation gate: with a baseline journal, a core that was already
// stopped when the upgrade was staged must not block promotion (the old
// all-cores rule made such nodes roll back every upgrade); a core that
// was running and is now down still blocks. Legacy journals without a
// baseline keep the strict rule.
func TestUpgradeBlockerBaseline(t *testing.T) {
	newAgent := func(cores map[model.CoreType]*managed) *Agent {
		a := &Agent{
			cfg:   Config{Supervised: true, DataDir: t.TempDir(), UpdateExecutable: filepath.Join(t.TempDir(), "polaris-agent")},
			log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			cores: cores,
		}
		a.rev.Store(1)
		return a
	}
	mk := func(running bool) *managed { return &managed{adapter: stubCoreAdapter{running: running}} }
	// Past the core gate, the test binary is not the staged candidate, so
	// the identity check is the expected next blocker.
	const pastCores = "当前进程不是待确认的候选版本"

	a := newAgent(map[model.CoreType]*managed{model.CoreXray: mk(true), model.CoreSingbox: mk(false)})
	base := &upgradeTrial{Baseline: true, WasRunning: []string{string(model.CoreXray)}, Version: Version, Candidate: "candidate"}
	if got := a.upgradeBlocker(base); got != pastCores {
		t.Fatalf("pre-stopped non-baseline core blocked confirmation: %q", got)
	}
	down := &upgradeTrial{Baseline: true, WasRunning: []string{string(model.CoreXray), string(model.CoreSingbox)}, Version: Version, Candidate: "candidate"}
	if got := a.upgradeBlocker(down); got != "内核 singbox 未运行" {
		t.Fatalf("baseline core down should block: %q", got)
	}
	legacy := &upgradeTrial{Version: Version, Candidate: "candidate"}
	if got := a.upgradeBlocker(legacy); got != "内核 singbox 未运行" {
		t.Fatalf("legacy journal should require every core: %q", got)
	}
	// A baseline core that left the applied spec no longer blocks.
	a2 := newAgent(map[model.CoreType]*managed{model.CoreSingbox: mk(true)})
	gone := &upgradeTrial{Baseline: true, WasRunning: []string{string(model.CoreXray)}, Version: Version, Candidate: "candidate"}
	if got := a2.upgradeBlocker(gone); got != pastCores {
		t.Fatalf("removed baseline core blocked confirmation: %q", got)
	}
}
