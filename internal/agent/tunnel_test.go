package agent

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core/cloudflared"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func testManagedSpec() (*model.NodeSpec, []byte) {
	tunnel := model.TunnelSpec{ProfileID: "fixture", ID: "12345678-abcd-1234-abcd-123456789abc", CredentialVersion: strings.Repeat("a", 16), Routes: []model.TunnelRoute{{Hostname: "cdn.example.com", Path: "^/ws$", Service: "http://127.0.0.1:18080"}, {Service: "http_status:404"}}}
	raw, _ := json.Marshal(map[string]string{"AccountTag": strings.Repeat("b", 32), "TunnelID": tunnel.ID, "TunnelSecret": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("agent-test-private", 3)))})
	return &model.NodeSpec{NodeID: "n", Rev: 1, Tunnels: []model.TunnelSpec{tunnel}, TunnelCredentials: map[string]json.RawMessage{"fixture": raw}}, raw
}
func TestManagedTunnelPrivateCacheAndHistory(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			a := &Agent{cfg: Config{DataDir: t.TempDir()}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			spec, raw := testManagedSpec()
			if remote {
				spec.Tunnels[0].CredentialMode = "remote"
				spec.Tunnels[0].Routes = nil
				payload, _ := json.Marshal(map[string]any{"a": strings.Repeat("b", 32), "t": spec.Tunnels[0].ID, "s": []byte(strings.Repeat("remote-fixture-private", 2))})
				token := base64.StdEncoding.EncodeToString(payload)
				raw, _ = json.Marshal(token)
				spec.TunnelCredentials["fixture"] = raw
			}
			if err := a.prepareTunnelCredentials(spec); err != nil {
				t.Fatal(err)
			}
			path := cloudflared.CredentialPath(filepath.Join(a.cfg.DataDir, "run", "cloudflared"), spec.Tunnels[0])
			fi, err := os.Stat(path)
			if err != nil || fi.Mode().Perm() != 0600 {
				t.Fatal("unsafe credential file", err)
			}
			if remote {
				b, _ := os.ReadFile(path)
				var token string
				_ = json.Unmarshal(raw, &token)
				if string(b) != token || !strings.HasSuffix(path, ".token") {
					t.Fatal("token file must contain raw token")
				}
			}
			a.saveSpec(spec)
			saved, err := os.ReadFile(a.specPath())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(saved), "TunnelSecret") || strings.Contains(string(saved), "tunnelCredentials") || strings.Contains(string(saved), string(raw)) {
				t.Fatal("credential persisted in spec history")
			}
			spec.TunnelCredentials = nil
			if err := a.prepareTunnelCredentials(spec); err != nil {
				t.Fatal("offline restore failed", err)
			}
			next := spec.Clone()
			next.Tunnels[0].CredentialVersion = strings.Repeat("c", 16)
			next.TunnelCredentials = map[string]json.RawMessage{"fixture": raw}
			if _, _, hot := userOnlyDiff(spec, next, model.CoreCloudflared); hot {
				t.Fatal("credential rotation skipped reload")
			}
			if err := a.prepareTunnelCredentials(next); err != nil {
				t.Fatal(err)
			}
			if err := a.pruneTunnelCredentials(next.Tunnels); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("old version retained after rotation")
			}
			if err := a.pruneTunnelCredentials(nil); err != nil {
				t.Fatal(err)
			}
			next.TunnelCredentials = nil
			if err := a.prepareTunnelCredentials(next); err == nil {
				t.Fatal("disabled credential resurrected without panel delivery")
			}
			restored := a.loadSpec()
			if restored == nil || len(restored.Tunnels) != 0 || restored.Rev != 0 {
				t.Fatal("old cache could resurrect a cleaned tunnel")
			}
		})
	}
}
func TestManagedTunnelCleanupFailureIsNotAcknowledged(t *testing.T) {
	a := newFakeAgent(t)
	root := filepath.Join(a.cfg.DataDir, "run", "cloudflared")
	os.MkdirAll(root, 0700)
	os.WriteFile(filepath.Join(root, "credentials"), []byte("corrupt directory"), 0600)
	result := a.Apply(t.Context(), &model.NodeSpec{NodeID: "n", Rev: 2})
	if result.ok || result.err == nil || a.appliedRev() != 0 || !result.retryable {
		t.Fatal("cleanup failure acknowledged", result)
	}
}

func TestManagedTunnelApplyStartsReloadsAndCleans(t *testing.T) {
	log := &callLog{}
	f := newFake(model.CoreCloudflared, log, false)
	a := newFakeAgent(t, f)
	spec, raw := testManagedSpec()
	spec.Cores = []model.CoreSpec{{Type: model.CoreCloudflared}}
	if result := a.Apply(t.Context(), spec); !result.ok {
		t.Fatal(result.err)
	}
	if !f.Health(t.Context()).Running || spec.TunnelCredentials != nil {
		t.Fatal("managed connector did not start or ephemeral secret retained")
	}
	oldPath := cloudflared.CredentialPath(filepath.Join(a.cfg.DataDir, "run", "cloudflared"), spec.Tunnels[0])
	next := spec.Clone()
	next.Rev = 2
	next.Tunnels[0].CredentialVersion = strings.Repeat("c", 16)
	next.TunnelCredentials = map[string]json.RawMessage{"fixture": raw}
	if result := a.Apply(t.Context(), next); !result.ok {
		t.Fatal(result.err)
	}
	if !strings.Contains(strings.Join(log.all(), ","), "cloudflared:reload") {
		t.Fatal("rotation skipped connector reload", log.all())
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("retired credential still present")
	}
	stop := next.Clone()
	stop.Rev = 3
	stop.Tunnels = nil
	if result := a.Apply(t.Context(), stop); !result.ok {
		t.Fatal(result.err)
	}
	if f.Health(t.Context()).Running {
		t.Fatal("disabled connector still running")
	}
	key := cloudflared.CredentialPath(filepath.Join(a.cfg.DataDir, "run", "cloudflared"), next.Tunnels[0])
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatal("disabled connector retained credential")
	}
}
