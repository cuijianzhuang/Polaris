package cloudflared

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

const fixtureSecret = "test-only-credential-never-log-this-string"

func fixtureTunnel() model.TunnelSpec {
	return model.TunnelSpec{ProfileID: "fixture", ID: "12345678-abcd-1234-abcd-123456789abc", CredentialVersion: strings.Repeat("a", 16), Routes: []model.TunnelRoute{{Hostname: "cdn.example.com", Path: "^/ws$", Service: "http://127.0.0.1:18080"}, {Service: "http_status:404"}}}
}
func fixtureCredential(t model.TunnelSpec) []byte {
	b, _ := json.Marshal(map[string]string{"AccountTag": strings.Repeat("a", 32), "TunnelID": t.ID, "TunnelSecret": base64.StdEncoding.EncodeToString([]byte(fixtureSecret))})
	return b
}
func fixtureAdapter(t *testing.T, bin string) (*Adapter, model.TunnelSpec) {
	t.Helper()
	work := t.TempDir()
	tunnel := fixtureTunnel()
	path := CredentialPath(work, tunnel)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := core.AtomicWrite(path, fixtureCredential(tunnel), 0600); err != nil {
		t.Fatal(err)
	}
	a := New(core.Options{WorkDir: work, BinPath: bin, Version: model.CloudflaredVersion, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), BindToParent: core.ParentBoundSupported()})
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, tunnel
}
func writeManifest(t *testing.T, a *Adapter, tunnels ...model.TunnelSpec) {
	t.Helper()
	cfg, err := a.Render(&model.NodeSpec{Tunnels: tunnels})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if err := core.AtomicWrite(filepath.Join(a.opt.WorkDir, "tunnels.json"), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "TunnelSecret") || strings.Contains(string(cfg), fixtureSecret) {
		t.Fatal("credential entered manifest")
	}
}
func TestCloudflaredConfigWithOfficialBinary(t *testing.T) {
	bin := os.Getenv("POLARIS_CLOUDFLARED_BIN")
	if bin == "" {
		t.Skip("official cloudflared not configured")
	}
	a, tunnel := fixtureAdapter(t, bin)
	writeManifest(t, a, tunnel)
	cfg, err := os.ReadFile(a.configPath(tunnel))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "TunnelSecret") || strings.Contains(string(cfg), fixtureSecret) {
		t.Fatal("plaintext in YAML")
	}
	fi, _ := os.Stat(a.configPath(tunnel))
	if fi.Mode().Perm() != 0600 {
		t.Fatal("configuration is not private")
	}
	c, err := a.makeChild(tunnel)
	if err != nil {
		t.Fatal(err)
	}
	args := append(append([]string{}, c.proc.Args...), "--help")
	output, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil || strings.Contains(string(output), "Incorrect Usage") {
		t.Fatal("official binary rejected managed command", err, string(output))
	}
}

// Executes as a stand-in child: validation is offline; run serves /ready and
// deliberately emits a pretend secret so logging/privacy assertions are real.
func TestCloudflaredChild(t *testing.T) {
	if os.Getenv("POLARIS_CLOUDFLARED_TEST_CHILD") != "1" {
		return
	}
	args := os.Args
	for _, arg := range args {
		if arg == "validate" {
			fmt.Println(fixtureSecret)
			os.Exit(0)
		}
	}
	for i, arg := range args {
		if arg == "--token-file" && i+1 < len(args) {
			raw, err := os.ReadFile(args[i+1])
			if err != nil {
				os.Exit(4)
			}
			if _, err := model.NormalizeRemoteTunnelToken(raw, fixtureTunnel().ID); err != nil {
				os.Exit(5)
			}
			fmt.Println(string(raw)) // The parent must consume, never log, token output.
		}
	}
	metrics := ""
	for i, arg := range args {
		if arg == "--metrics" && i+1 < len(args) {
			metrics = args[i+1]
		}
	}
	if metrics == "" {
		os.Exit(2)
	}
	if os.Getenv("TUNNEL_TOKEN") != "" || os.Getenv("TUNNEL_LOGFILE") != "" {
		os.Exit(3)
	}
	fmt.Println("unfiltered child output: " + fixtureSecret)
	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		b, _ := os.ReadFile(os.Getenv("POLARIS_CLOUDFLARED_READY_FILE"))
		status, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
	})
	if http.ListenAndServe(metrics, nil) != nil {
		os.Exit(2)
	}
}
func testBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POLARIS_CLOUDFLARED_TEST_CHILD", "1")
	script := filepath.Join(t.TempDir(), "cloudflared")
	// The test executable path contains no shell interpolation; quote defensively.
	body := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=^TestCloudflaredChild$ -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return script
}
func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("cloudflared state did not converge")
}
func TestCloudflaredSupervisionPrivacyAndReadiness(t *testing.T) {
	bin := testBinary(t)
	t.Setenv("TUNNEL_TOKEN", "inherited-token-must-not-reach-child")
	t.Setenv("TUNNEL_LOGFILE", filepath.Join(t.TempDir(), "unexpected-log"))
	ready := filepath.Join(t.TempDir(), "ready")
	os.WriteFile(ready, []byte("200"), 0600)
	t.Setenv("POLARIS_CLOUDFLARED_READY_FILE", ready)
	a, tunnel := fixtureAdapter(t, bin)
	writeManifest(t, a, tunnel)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { st := a.Health(context.Background()); return st.Running && st.Error == "" })
	first := a.Health(context.Background()).PID
	if strings.Contains(strings.Join(a.Logs(500), "\n"), fixtureSecret) {
		t.Fatal("child output leaked")
	}
	os.WriteFile(ready, []byte("503"), 0600)
	eventually(t, func() bool { st := a.Health(context.Background()); return st.Running && st.Error != "" })
	if err := syscall.Kill(first, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { st := a.Health(context.Background()); return st.Running && st.PID != first })
	os.WriteFile(ready, []byte("200"), 0600)
	eventually(t, func() bool { return a.Health(context.Background()).Error == "" })
	// Route changes restart this connector and use a new immutable config.
	before := a.Health(context.Background()).PID
	tunnel.Routes[0].Path = "^/new$"
	writeManifest(t, a, tunnel)
	if err := a.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Health(context.Background()).PID == before {
		t.Fatal("route change did not replace connector")
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if a.Health(context.Background()).Running {
		t.Fatal("stop left a process running")
	}
}
