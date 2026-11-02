package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/cloudflared"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func (a *Agent) prepareTunnelCredentials(spec *model.NodeSpec) error {
	if len(spec.Tunnels) == 0 {
		return nil
	}
	if a.cfg.DataDir == "" || len(spec.Tunnels) > 32 {
		return fmt.Errorf("invalid managed tunnel storage or count")
	}
	work := filepath.Join(a.cfg.DataDir, "run", "cloudflared")
	data := map[string][]byte{}
	seen := map[string]bool{}
	for _, t := range spec.Tunnels {
		if err := t.Validate(); err != nil {
			return err
		}
		if seen[t.ProfileID] || seen[t.ID] {
			return fmt.Errorf("duplicate managed tunnel")
		}
		seen[t.ProfileID] = true
		seen[t.ID] = true
		raw := spec.TunnelCredentials[t.ProfileID]
		if len(raw) > 0 && t.CredentialMode == "remote" {
			var token string
			if json.Unmarshal(raw, &token) != nil {
				return fmt.Errorf("Tunnel Token 下发格式无效")
			}
			raw = []byte(token)
		}
		path := cloudflared.CredentialPath(work, t)
		if len(raw) == 0 {
			var err error
			raw, err = os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("Tunnel 凭据尚未下发，请连接面板同步最新配置")
			}
		}
		normalized, err := model.NormalizeManagedTunnelCredential(raw, t.ID, t.CredentialMode)
		if err != nil {
			return fmt.Errorf("Tunnel 凭据无效，请重新导入")
		}
		if prior, err := os.ReadFile(path); err == nil && !bytes.Equal(prior, normalized) {
			return fmt.Errorf("Tunnel 凭据版本不一致，请重新同步")
		}
		data[path] = normalized
	}
	for path, raw := range data {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return fmt.Errorf("创建 Tunnel 凭据目录失败")
		}
		// Parents are private too; no plaintext is written to a public run directory.
		if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := core.AtomicWrite(path, raw, 0600); err != nil {
			return fmt.Errorf("保存 Tunnel 私有凭据失败")
		}
	}
	return nil
}

func (a *Agent) pruneTunnelCredentials(keep []model.TunnelSpec) error {
	if a.cfg.DataDir == "" {
		return nil
	}
	root := filepath.Join(a.cfg.DataDir, "run", "cloudflared", "credentials")
	wanted := map[string]bool{}
	for _, t := range keep {
		if t.Validate() == nil {
			wanted[cloudflared.CredentialPath(filepath.Dir(root), t)] = true
		}
	}
	profiles, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("Tunnel 私有凭据清理失败，等待重新同步")
	}
	for _, profile := range profiles {
		dir := filepath.Join(root, profile.Name())
		if !profile.IsDir() {
			if err := os.Remove(dir); err != nil {
				return fmt.Errorf("Tunnel 私有凭据清理失败")
			}
			continue
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("Tunnel 私有凭据清理失败")
		}
		retained := false
		for _, file := range files {
			path := filepath.Join(dir, file.Name())
			if wanted[path] {
				retained = true
				continue
			}
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("Tunnel 私有凭据清理失败，等待重新同步")
			}
		}
		if !retained {
			if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("Tunnel 私有凭据清理失败")
			}
		}
	}
	return nil
}
