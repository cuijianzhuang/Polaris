//go:build licensedev

// polaris-license-dev creates keys and signs licenses for local testing.
// It is not part of releases (build with -tags licensedev); production
// licenses come from the license server, which keeps the private key.
//
//	go run -tags licensedev ./cmd/polaris-license-dev keygen
//	go run -tags licensedev ./cmd/polaris-license-dev sign -key <private b64> -instance inst_… -plan pro -days 365
//
// Run the panel with POLARIS_LICENSE_PUBKEYS=<public b64> (development
// builds only) to accept these licenses.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/license"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: polaris-license-dev keygen | sign [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "keygen":
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		fmt.Println("public: ", base64.StdEncoding.EncodeToString(pub))
		fmt.Println("private:", base64.StdEncoding.EncodeToString(priv))
	case "sign":
		fs := flag.NewFlagSet("sign", flag.ExitOnError)
		key := fs.String("key", "", "private key (base64)")
		inst := fs.String("instance", "", "bind to this instance id (empty = any)")
		plan := fs.String("plan", "pro", "pro / enterprise")
		customer := fs.String("customer", "dev", "customer name")
		days := fs.Int("days", 365, "valid for N days (0 = perpetual)")
		updates := fs.Int("updates", 0, "updates for N days (0 = all versions)")
		nodes := fs.Int("nodes", 0, "node cap (0 = unlimited)")
		users := fs.Int("users", 0, "user cap (0 = unlimited)")
		features := fs.String("features", strings.Join(license.AllFeatures, ","), "comma-separated features")
		offline := fs.Bool("offline", false, "never contact the license server")
		fs.Parse(os.Args[2:])
		raw, err := base64.StdEncoding.DecodeString(*key)
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			fmt.Fprintln(os.Stderr, "-key must be a base64 Ed25519 private key (see keygen)")
			os.Exit(2)
		}
		now := time.Now()
		l := &license.License{ID: "lic_dev_" + now.Format("20060102150405"), Customer: *customer, Plan: *plan,
			Limits: license.Limits{Nodes: *nodes, Users: *users}, Features: strings.Split(*features, ","),
			Instance: *inst, Offline: *offline, IssuedAt: now.Format("2006-01-02")}
		if *days > 0 {
			l.ExpiresAt = now.AddDate(0, 0, *days).Format("2006-01-02")
		}
		if *updates > 0 {
			l.UpdatesUntil = now.AddDate(0, 0, *updates).Format("2006-01-02")
		}
		text, err := license.Sign(l, ed25519.PrivateKey(raw))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(text)
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
}
