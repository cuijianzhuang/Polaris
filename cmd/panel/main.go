// Command polaris is the Polaris control panel.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cuijianzhuang/polaris/internal/panel"
	"github.com/cuijianzhuang/polaris/web"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "admin", "backup", "restore", "restore-drill", "node-token", "migrate":
			os.Exit(maintenance(os.Args[1], os.Args[2:]))
		}
	}
	cfgPath := flag.String("config", os.Getenv("POLARIS_CONFIG"), "path to config.yaml (optional)")
	showVersion := flag.Bool("version", false, "print version and exit")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()
	if *showVersion {
		// install-panel.sh reads the third field (builds before the split print none)
		fmt.Println("polaris", panel.Version, panel.Edition())
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
  polaris admin reset-passkeys [-u NAME]    revoke passkeys, account recovery codes and sessions
  polaris admin license [show|install FILE|deactivate]
                                            show, install or release this panel's license
  polaris backup FILE [-passphrase P]       backup archive (SQLite or PostgreSQL); encrypted with a
                                            passphrase it carries the secret key and restores anywhere
  polaris restore-drill FILE [-passphrase P]  validate an isolated restore and print a JSON report
  polaris restore FILE [-passphrase P]      replace the data with a backup (stop the panel first)
  polaris migrate -to DSN [-replace]       copy all data to another database (e.g. SQLite ->
                                            postgres://user:pass@host/db); stop the panel first
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
	to := fs.String("to", "", "migrate: target database DSN")
	replace := fs.Bool("replace", false, "migrate: overwrite a target that already has data")
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
	case cmd == "restore-drill":
		if len(pos) != 1 {
			fs.Usage()
			return 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		result, err := panel.DrillRestore(ctx, cfg, pos[0], *passphrase)
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return fail(err)
		}
		if result.Status != "passed" {
			return 1
		}
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
	case cmd == "migrate":
		if *to == "" || len(pos) != 0 {
			fs.Usage()
			return 2
		}
		res, err := panel.Migrate(cfg, *to, *replace)
		if err != nil {
			return fail(err)
		}
		for _, t := range res.Tables {
			if t.Source > 0 {
				fmt.Printf("  %-24s %d\n", t.Name, t.Target)
			}
		}
		for _, c := range res.Truncated {
			fmt.Fprintln(os.Stderr, "warning: values longer than the column were cut:", c)
		}
		fmt.Printf("migrated %d rows; row counts match.\nNext: set POLARIS_DB_DSN to the target, then start the panel.\n", res.Rows)
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
	case sub == "license":
		action, file := "show", ""
		if len(pos) > 0 {
			action = pos[0]
		}
		if len(pos) > 1 {
			file = pos[1]
		}
		out, err := panel.AdminLicense(cfg, action, file)
		if err != nil {
			return fail(err)
		}
		fmt.Println(out)
	case sub == "reset-passkeys":
		name, err := panel.AdminResetPasskeys(cfg, *user)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("passkeys, account recovery codes and sessions revoked for %s\n", name)
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
