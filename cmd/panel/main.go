// Command polaris is the Polaris control panel.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cuijianzhuang/polaris/internal/panel"
	"github.com/cuijianzhuang/polaris/web"
)

func main() {
	cfgPath := flag.String("config", os.Getenv("POLARIS_CONFIG"), "path to config.yaml (optional)")
	showVersion := flag.Bool("version", false, "print version and exit")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()
	if *showVersion {
		fmt.Println("polaris", panel.Version)
		return
	}
	lvl := slog.LevelInfo
	if *debug {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	cfg, err := panel.LoadConfig(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	srv, err := panel.New(cfg, log, panel.WebFS(web.FS()))
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
