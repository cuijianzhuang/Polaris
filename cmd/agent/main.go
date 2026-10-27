// Command polaris-agent runs on every node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cuijianzhuang/polaris/internal/agent"
)

func main() {
	panelURL := flag.String("panel", os.Getenv("POLARIS_PANEL"), "panel agent endpoint, e.g. https://panel.example.com:9443")
	token := flag.String("token", "", "one-time install token (first run only)")
	caSHA := flag.String("ca-sha256", "", "panel CA fingerprint (first run only)")
	dataDir := flag.String("data", envOr("POLARIS_DATA", "/var/lib/polaris"), "data directory")
	mirror := flag.String("mirror", os.Getenv("POLARIS_CORE_MIRROR"), "GitHub download mirror prefix for core releases")
	unverified := flag.Bool("allow-unverified", false, "allow installing cores whose release publishes no SHA-256")
	noIP := flag.Bool("no-ip-detect", false, "disable public IP detection used by DDNS")
	enrollOnly := flag.Bool("enroll-only", false, "enrol with --token and exit (used by install.sh)")
	showVersion := flag.Bool("version", false, "print version and exit")
	worker := flag.Bool("agent-worker", false, "internal supervised worker")
	entry := flag.String("agent-entry", "", "internal stable executable")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()
	exe, _ := os.Executable() // before any self-update replaces the file
	if *showVersion {
		fmt.Println("polaris-agent", agent.Version)
		return
	}
	lvl := slog.LevelInfo
	if *debug {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	urlFile := filepath.Join(*dataDir, "agent", "panel-url")
	if *panelURL == "" {
		if b, err := os.ReadFile(urlFile); err == nil {
			*panelURL = strings.TrimSpace(string(b))
		}
	}
	if *panelURL == "" {
		log.Error("--panel is required")
		os.Exit(2)
	}
	if *token != "" {
		id, err := agent.Enroll(ctx, *panelURL, *token, *caSHA, *dataDir, agent.Version)
		if err != nil {
			log.Error("enroll failed", "err", err)
			os.Exit(1)
		}
		_ = os.WriteFile(urlFile, []byte(*panelURL), 0o600)
		log.Info("enrolled", "node", id.NodeID)
		if *enrollOnly {
			return
		}
	}
	if !*worker {
		if exe == "" {
			log.Error("cannot locate executable")
			os.Exit(1)
		}
		var err error
		exe, err = filepath.EvalSymlinks(exe)
		if err != nil {
			log.Error("locate executable", "err", err)
			os.Exit(1)
		}
		args := []string{"--agent-worker", "--agent-entry", exe, "--panel", *panelURL, "--data", *dataDir, "--mirror", *mirror}
		if *unverified {
			args = append(args, "--allow-unverified")
		}
		if *noIP {
			args = append(args, "--no-ip-detect")
		}
		if *debug {
			args = append(args, "--debug")
		}
		if err := agent.Supervise(ctx, exe, *dataDir, args, log); err != nil {
			log.Error("agent supervisor", "err", err)
			os.Exit(1)
		}
		return
	}
	a, err := agent.New(agent.Config{Supervised: true, UpdateExecutable: *entry, PanelURL: *panelURL, DataDir: *dataDir, Mirror: *mirror, AllowUnverified: *unverified, DisableIPDetect: *noIP, ToolPath: "/usr/local/bin/polaris"}, log)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	if err := a.Run(ctx); err != nil {
		if errors.Is(err, agent.ErrRestart) {
			log.Info("handing staged upgrade to supervisor")
			os.Exit(agent.UpgradeRestartExit)
		}
		log.Error("agent", "err", err)
		os.Exit(1)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
