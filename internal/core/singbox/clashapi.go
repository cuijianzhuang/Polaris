package singbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

// clashSecret returns the Clash API secret kept in the work dir, so a
// running sing-box keeps accepting it after the agent restarts.
func clashSecret(dir string) string {
	if dir == "" {
		return ""
	}
	f := filepath.Join(dir, "clash-api.secret")
	if b, err := os.ReadFile(f); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
		return strings.TrimSpace(string(b))
	}
	b := make([]byte, 16)
	rand.Read(b)
	s := hex.EncodeToString(b)
	os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
		return ""
	}
	return s
}

type clashConn struct {
	ID       string   `json:"id"`
	Upload   int64    `json:"upload"`
	Download int64    `json:"download"`
	Chains   []string `json:"chains"`
	Metadata struct {
		Type string `json:"type"` // "<protocol>/<inbound tag>"
	} `json:"metadata"`
}

// clashTraffic diffs the Clash API per-connection counters into inbound and
// outbound totals. The API names no user, so per-user traffic stays
// unavailable; bytes a connection moves between its last poll and closing
// are not counted.
func (a *Adapter) clashTraffic(ctx context.Context) ([]model.TrafficDelta, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.opt.APIListen+"/connections", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sing-box clash api: %s", resp.Status)
	}
	var body struct {
		Connections []clashConn `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	per := map[string][2]int64{}
	seen := make(map[string][2]int64, len(body.Connections))
	add := func(key string, up, down int64) {
		v := per[key]
		v[0] += up
		v[1] += down
		per[key] = v
	}
	for _, c := range body.Connections {
		prev := a.conns[c.ID]
		seen[c.ID] = [2]int64{c.Upload, c.Download}
		up, down := c.Upload-prev[0], c.Download-prev[1]
		if up < 0 || down < 0 {
			up, down = c.Upload, c.Download
		}
		if up == 0 && down == 0 {
			continue
		}
		if _, tag, ok := strings.Cut(c.Metadata.Type, "/"); ok && tag != "" {
			add(core.TagPrefixInbound+tag, up, down)
		}
		if len(c.Chains) > 0 && c.Chains[0] != "" { // the outbound used; groups follow it
			add(core.TagPrefixOutbound+c.Chains[0], up, down)
		}
	}
	a.conns = seen
	out := make([]model.TrafficDelta, 0, len(per))
	for k, v := range per {
		out = append(out, model.TrafficDelta{UserID: k, Up: v[0], Down: v[1]})
	}
	return out, nil
}
