package cloudflared

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"gopkg.in/yaml.v3"
)

func remoteFixture(t *testing.T, bin string) (*Adapter, model.TunnelSpec, string) {
	t.Helper()
	a, tunnel := fixtureAdapter(t, bin)
	tunnel.CredentialMode = "remote"
	tunnel.Routes = nil
	b, _ := json.Marshal(map[string]any{"a": strings.Repeat("a", 32), "t": tunnel.ID, "s": []byte(fixtureSecret)})
	token := base64.StdEncoding.EncodeToString(b)
	if err := core.AtomicWrite(CredentialPath(a.opt.WorkDir, tunnel), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	return a, tunnel, token
}
func TestRemoteTunnelConfigAndOfficialCommand(t *testing.T) {
	bin := os.Getenv("POLARIS_CLOUDFLARED_BIN")
	a, tunnel, token := remoteFixture(t, bin)
	writeManifest(t, a, tunnel)
	b, err := os.ReadFile(a.configPath(tunnel))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if yaml.Unmarshal(b, &cfg) != nil || len(cfg) != 1 || cfg["tunnel"] != tunnel.ID {
		t.Fatal("remote config overrides cloud routes")
	}
	c, err := a.makeChild(tunnel)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(c.proc.Args, " ")
	if !strings.Contains(args, "run --token-file "+CredentialPath(a.opt.WorkDir, tunnel)) || strings.Contains(args, token) || strings.Contains(string(b), token) {
		t.Fatal("token file not used or token leaked")
	}
	if bin != "" {
		out, err := exec.Command(bin, append(append([]string{}, c.proc.Args...), "--help")...).CombinedOutput()
		if err != nil || strings.Contains(string(out), "Incorrect Usage") || !strings.Contains(string(out), "--token-file") {
			t.Fatal("official binary rejected remote command", err, string(out))
		}
	}
}
func TestRemoteTunnelTokenFileSupervisionAndRotation(t *testing.T) {
	a, tunnel, token := remoteFixture(t, testBinary(t))
	t.Setenv("TUNNEL_TOKEN", "wrong-inherited-token")
	writeManifest(t, a, tunnel)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { st := a.Health(context.Background()); return st.Running && st.Error == "" })
	pid := a.Health(context.Background()).PID
	if strings.Contains(strings.Join(a.Logs(500), "\n"), token) {
		t.Fatal("raw child output leaked token")
	}
	tunnel.CredentialVersion = strings.Repeat("c", 16)
	if err := core.AtomicWrite(CredentialPath(a.opt.WorkDir, tunnel), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, a, tunnel)
	if err := a.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		st := a.Health(context.Background())
		return st.Running && st.Error == "" && st.PID != pid
	})
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Health(context.Background()).Running {
		t.Fatal("remote connector still running")
	}
	if _, err := os.Stat(filepath.Join(a.opt.WorkDir, "unexpected-log")); !os.IsNotExist(err) {
		t.Fatal("unexpected child output")
	}
}

func TestRemoteTunnelRejectsLegacyCloudflared(t *testing.T) {
	a, tunnel, _ := remoteFixture(t, "")
	a.opt.Version = "2025.3.0"
	if _, err := a.Render(&model.NodeSpec{Tunnels: []model.TunnelSpec{tunnel}}); err == nil {
		t.Fatal("binary without --token-file accepted")
	}
}
