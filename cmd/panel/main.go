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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "admin", "backup", "restore", "node-token":
			os.Exit(maintenance(os.Args[1], os.Args[2:]))
		}
	}
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

const usage = `usage:
  polaris                                   run the panel
  polaris admin list                        list admin accounts
  polaris admin reset-password [-u NAME] [-p PASSWORD]
                                            set a new password (random if -p is empty), unlock
                                            the account and sign out every session
  polaris admin reset-2fa [-u NAME]         turn off two-factor authentication
  polaris backup FILE [-passphrase P]       backup archive (SQLite or PostgreSQL); encrypted with a
                                            passphrase it carries the secret key and restores anywhere
  polaris restore FILE [-passphrase P]      replace the data with a backup (stop the panel first)
  polaris node-token [-n NAME]              one-time install token for a node on this server
                                            (creates the node "本机" unless it exists)
Configuration is read from the environment (POLARIS_*) or -config FILE.`

// maintenance runs operator subcommands against the configured database.
func maintenance(cmd string, args []string) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fs.String("config", os.Getenv("POLARIS_CONFIG"), "path to config.yaml")
	user := fs.String("u", "", "admin username (default: the first admin)")
	pass := fs.String("p", "", "new password (default: random)")
	nodeName := fs.String("n", "", "node name (default: 本机)")
	passphrase := fs.String("passphrase", os.Getenv("POLARIS_BACKUP_PASSPHRASE"), "backup passphrase")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	sub := ""
	if cmd == "admin" {
		if len(args) == 0 {
			fs.Usage()
			return 2
		}
		sub, args = args[0], args[1:]
	}
	// Flags may follow the file name (polaris restore FILE -passphrase P).
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	cfg, err := panel.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	fail := func(err error) int { fmt.Fprintln(os.Stderr, "error:", err); return 1 }
	switch {
	case cmd == "backup":
		if len(pos) != 1 {
			fs.Usage()
			return 2
		}
		if err := panel.Backup(cfg, pos[0], *passphrase); err != nil {
			return fail(err)
		}
		fmt.Println("backup written to", pos[0])
	case cmd == "restore":
		if len(pos) != 1 {
			fs.Usage()
			return 2
		}
		snap, err := panel.Restore(cfg, pos[0], *passphrase)
		if err != nil {
			return fail(err)
		}
		fmt.Println("restored; the previous data was saved to", snap)
	case cmd == "node-token":
		tok, created, err := panel.LocalNodeToken(cfg, *nodeName)
		if err != nil {
			return fail(err)
		}
		if created {
			fmt.Fprintln(os.Stderr, "node created")
		}
		fmt.Println(tok)
	case sub == "list":
		names, err := panel.AdminList(cfg)
		if err != nil {
			return fail(err)
		}
		for _, n := range names {
			fmt.Println(n)
		}
	case sub == "reset-password":
		name, pw, err := panel.AdminResetPassword(cfg, *user, *pass)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("username: %s\npassword: %s\n", name, pw)
	case sub == "reset-2fa":
		name, err := panel.AdminDisable2FA(cfg, *user)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("two-factor authentication disabled for %s\n", name)
	default:
		fs.Usage()
		return 2
	}
	return 0
}
