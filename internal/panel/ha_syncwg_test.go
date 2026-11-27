package panel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestParseWGDump parses a `wg show wg0 dump` transcript: the first line is
// the interface, the rest are peers; short lines are skipped, never fatal.
func TestParseWGDump(t *testing.T) {
	out := "abcdef1234= (none) 51820 off\n" +
		"peerAAA= (none) 203.0.113.1:51820 10.8.0.2/32 1700000000 0 0 25\n" +
		"peerBBB= (none) (none) 10.8.0.3/32 0 0 0 off\n" +
		"garbage\n"
	have := parseWGDump(out)
	if len(have) != 2 {
		t.Fatalf("peers: %v", have)
	}
	if have["peerAAA="].Endpoint != "203.0.113.1:51820" || have["peerAAA="].AllowedIPs != "10.8.0.2/32" {
		t.Fatalf("peerAAA: %+v", have["peerAAA="])
	}
	if have["peerBBB="].Endpoint != "(none)" {
		t.Fatalf("peerBBB: %+v", have["peerBBB="])
	}
	if len(parseWGDump("")) != 0 {
		t.Fatal("empty dump should parse to no peers")
	}
}

func TestDiffWGPeers(t *testing.T) {
	want := []store.HAPeer{
		{WGIP: "10.8.0.2", Pubkey: "peerNEW=", Endpoint: "203.0.113.1"},
		{WGIP: "10.8.0.3", Pubkey: "peerOK=", Endpoint: "203.0.113.2"},
		{WGIP: "10.8.0.4", Pubkey: "peerDrift=", Endpoint: "203.0.113.3"},
		{WGIP: "10.8.0.5", Pubkey: "peerNoIP=", Endpoint: "203.0.113.4"},
	}
	have := map[string]wgPeerState{
		"peerOK=":    {Pubkey: "peerOK=", Endpoint: "203.0.113.2:51820", AllowedIPs: "10.8.0.3/32"},
		"peerDrift=": {Pubkey: "peerDrift=", Endpoint: "198.51.100.9:51820", AllowedIPs: "10.8.0.4/32"},
		"peerNoIP=":  {Pubkey: "peerNoIP=", Endpoint: "203.0.113.4:51820", AllowedIPs: "10.8.0.9/32"},
		"peerGone=":  {Pubkey: "peerGone=", Endpoint: "203.0.113.5:51820", AllowedIPs: "10.8.0.6/32"},
	}
	add, remove := diffWGPeers(want, have)
	addKeys := []string{}
	for _, p := range add {
		addKeys = append(addKeys, p.Pubkey)
	}
	if strings.Join(addKeys, ",") != "peerNEW=,peerDrift=,peerNoIP=" {
		t.Fatalf("add: %v", addKeys)
	}
	if len(remove) != 1 || remove[0] != "peerGone=" {
		t.Fatalf("remove: %v", remove)
	}
	if add, remove := diffWGPeers(want, map[string]wgPeerState{}); len(add) != 4 || len(remove) != 0 {
		t.Fatalf("empty live state should add all: +%d -%d", len(add), len(remove))
	}
}

// TestRewriteWGConf keeps the [Interface] section verbatim, drops old peers
// and appends the wanted ones; the file stays 0600 and the write is atomic
// (no .tmp file left behind).
func TestRewriteWGConf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wg0.conf")
	orig := "# comment\n[Interface]\nPrivateKey = SECRET\nAddress = 10.8.0.1/24\nListenPort = 51820\n\n[Peer]\nPublicKey = oldpeer=\nAllowedIPs = 10.8.0.2/32\n"
	if err := os.WriteFile(path, []byte(orig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := rewriteWGConf(path, []store.HAPeer{
		{WGIP: "10.8.0.2", Pubkey: "newpeer=", Endpoint: "203.0.113.1"},
		{WGIP: "10.8.0.3", Pubkey: "noendpoint=", Endpoint: ""},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "# comment\n[Interface]\nPrivateKey = SECRET\nAddress = 10.8.0.1/24\nListenPort = 51820\n") {
		t.Fatalf("interface section not kept verbatim:\n%s", s)
	}
	if strings.Contains(s, "oldpeer=") {
		t.Fatalf("old peer not dropped:\n%s", s)
	}
	if !strings.Contains(s, "[Peer]\nPublicKey = newpeer=\nAllowedIPs = 10.8.0.2/32\nEndpoint = 203.0.113.1:51820\nPersistentKeepalive = 25\n") {
		t.Fatalf("new peer block missing:\n%s", s)
	}
	if strings.Count(s, "Endpoint =") != 1 {
		t.Fatalf("peer without endpoint must not get an Endpoint line:\n%s", s)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("mode: %v %v", fi, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file left behind")
	}
}

// TestSyncWGWithFakeBinary runs the whole reconciler against a fake `wg`
// script: the interface reports up, the dump lists one stale peer, so the
// wanted peer is added, the stale one removed, and wg0.conf rewritten.
func TestSyncWGWithFakeBinary(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "wg.log")
	script := "#!/bin/sh\n" +
		"echo \"wg $*\" >> \"$FAKEWG_LOG\"\n" +
		"case \"$*\" in\n" +
		"  \"show wg0 dump\") printf 'ifacekey= (none) 51820 off\\noldpeer= (none) 198.51.100.9:51820 10.8.0.9/32 0 0 0 off\\n' ;;\n" +
		"esac\n" +
		"exit 0\n"
	wgPath := filepath.Join(binDir, "wg")
	if err := os.WriteFile(wgPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKEWG_LOG", log)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	confPath := filepath.Join(dir, "wg0.conf")
	if err := os.WriteFile(confPath, []byte("[Interface]\nPrivateKey = SECRET\nAddress = 10.8.0.1/24\nListenPort = 51820\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{DataDir: dir, SecretKey: "x"}
	cfg.DB.DSN = filepath.Join(dir, "syncwg.db")
	box, err := secret.NewBoxSet("x", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DB.DSN, box, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.HAPeer{WGIP: "10.8.0.2", Pubkey: "newpeer=", Endpoint: "203.0.113.1", Role: "secondary"}).Error; err != nil {
		t.Fatal(err)
	}
	st.Close()

	oldBinary, oldConf := haWGBinary, haWGConfPath
	haWGBinary, haWGConfPath = "wg", confPath
	defer func() { haWGBinary, haWGConfPath = oldBinary, oldConf }()

	summary, err := SyncWG(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "wireguard peers: +1 -1 (1 total)" {
		t.Fatalf("summary: %q", summary)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	c := string(calls)
	for _, want := range []string{
		"wg show wg0\n",
		"wg show wg0 dump\n",
		"wg set wg0 peer newpeer= allowed-ips 10.8.0.2/32 endpoint 203.0.113.1:51820 persistent-keepalive 25\n",
		"wg set wg0 peer oldpeer= remove\n",
	} {
		if !strings.Contains(c, want) {
			t.Fatalf("missing wg call %q in:\n%s", want, c)
		}
	}
	conf, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), "PublicKey = newpeer=") || strings.Contains(string(conf), "oldpeer=") {
		t.Fatalf("conf not reconciled:\n%s", conf)
	}
}

// TestHASealSecretRoundTrip seals a value through the operator CLI and
// reads it back: the sealed row opens to the original value, and the row
// itself never contains the plaintext.
func TestHASealSecretRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DataDir: dir, SecretKey: "x"}
	cfg.DB.DSN = filepath.Join(dir, "seal.db")
	if err := HASealSecret(cfg, "ha_repl_password", "s3cr3t-repl"); err != nil {
		t.Fatal(err)
	}
	if err := HASealSecret(cfg, "ha_repl_password", "s3cr3t-repl-v2"); err != nil {
		t.Fatal(err)
	}
	box, err := secret.NewBoxSet("x", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DB.DSN, box, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var row store.ClusterSecret
	if err := st.DB.First(&row, "name = ?", "ha_repl_password").Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.Value, "s3cr3t-repl-v2") {
		t.Fatal("sealed value contains the plaintext")
	}
	if got := st.Box.MustOpen(row.Value); got != "s3cr3t-repl-v2" {
		t.Fatalf("unsealed: %q", got)
	}
	var n int64
	st.DB.Model(&store.ClusterSecret{}).Where("name = ?", "ha_repl_password").Count(&n)
	if n != 1 {
		t.Fatalf("rows: %d", n)
	}
}

func TestPeerLastSeenUpdates(t *testing.T) {
	joined := time.Unix(1700000000, 0).UTC()
	peers := []store.HAPeer{
		{WGIP: "10.8.0.2", Pubkey: "peerAAA=", LastSeen: &joined},
		{WGIP: "10.8.0.3", Pubkey: "peerBBB="},
		{WGIP: "10.8.0.4", Pubkey: "peerCCC=", LastSeen: &joined},
	}
	have := parseWGDump("pub (none) 51820 off\n" +
		"peerAAA= (none) 203.0.113.1:51820 10.8.0.2/32 1700003600 0 0 25\n" +
		"peerBBB= (none) (none) 10.8.0.3/32 0 0 0 off\n" +
		"peerCCC= (none) 203.0.113.3:51820 10.8.0.4/32 1699990000 0 0 25\n")
	if have["peerAAA="].Handshake != 1700003600 {
		t.Fatalf("handshake not parsed: %+v", have["peerAAA="])
	}
	ups := peerLastSeenUpdates(peers, have)
	// AAA advanced past its join time; BBB never handshook (0) so no
	// update; CCC's handshake predates the stored value, never backwards.
	if len(ups) != 1 || !ups["10.8.0.2"].Equal(time.Unix(1700003600, 0).UTC()) {
		t.Fatalf("updates: %v", ups)
	}
	// A peer with no stored value takes its first observed handshake.
	peers[1].LastSeen = nil
	have["peerBBB="] = wgPeerState{Pubkey: "peerBBB=", Handshake: 1700007200}
	ups = peerLastSeenUpdates(peers, have)
	if !ups["10.8.0.3"].Equal(time.Unix(1700007200, 0).UTC()) {
		t.Fatalf("first handshake for BBB: %v", ups)
	}
}
