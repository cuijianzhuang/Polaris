package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

const maxGeoAsset = 256 << 20

// fetchGeoAssets downloads Xray .dat files into dir: the missing ones, or
// all of them with force. Xray then keeps them fresh on its own schedule.
// Failures are logged and reported; the core still starts without them.
func (a *Agent) fetchGeoAssets(ctx context.Context, dir string, assets []model.GeoAsset, force bool) []string {
	var done, errs []string
	for _, as := range assets {
		if as.File != filepath.Base(as.File) || !strings.HasPrefix(as.URL, "https://") {
			errs = append(errs, as.File+": 无效的文件名或地址")
			continue
		}
		path := filepath.Join(dir, as.File)
		if _, err := os.Stat(path); err == nil && !force {
			continue
		}
		if err := download(ctx, as.URL, path); err != nil {
			a.log.Warn("geodata download", "file", as.File, "err", err)
			errs = append(errs, fmt.Sprintf("%s: %v", as.File, err))
			continue
		}
		done = append(done, as.File)
	}
	if len(errs) > 0 {
		done = append(done, "失败："+strings.Join(errs, "; "))
	}
	return done
}

func download(ctx context.Context, url, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxGeoAsset+1))
	if err != nil {
		return err
	}
	if len(b) > maxGeoAsset {
		return errors.New("文件过大")
	}
	if len(b) == 0 {
		return errors.New("空文件")
	}
	return core.AtomicWrite(path, b, 0o644)
}

// updateGeodata is the geodata_update command: download now, then reload
// Xray so it reads the new files.
func (a *Agent) updateGeodata(ctx context.Context, args map[string]string) (string, error) {
	var assets []model.GeoAsset
	if err := json.Unmarshal([]byte(args["assets"]), &assets); err != nil || len(assets) == 0 {
		return "", errors.New("没有要更新的文件")
	}
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	m := a.cores[model.CoreXray]
	if m == nil || m.binDir == "" {
		return "", errors.New("该节点未运行 Xray")
	}
	res := a.fetchGeoAssets(ctx, m.binDir, assets, true)
	if len(res) > 0 && strings.HasPrefix(res[len(res)-1], "失败：") && len(res) == 1 {
		return "", errors.New(strings.TrimPrefix(res[0], "失败："))
	}
	if m.adapter.Health(ctx).Running {
		a.collectFrom(ctx, m)
		if err := m.adapter.Reload(ctx); err != nil {
			return strings.Join(res, ", "), fmt.Errorf("已下载，但重载 Xray 失败：%w", err)
		}
	}
	return "已更新 " + strings.Join(res, ", "), nil
}
