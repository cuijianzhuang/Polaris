package panel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Panel self-update. The panel runs unprivileged and cannot replace its own
// binary, so it writes <data>/update-request ("latest" or a tag); the root
// polaris-update.path unit installed by `polaris install/update` runs
// `polaris update-request`, which updates, restarts the panel and leaves
// <data>/update-result for the panel to report (Telegram, audit log) and,
// when asked, to follow with an upgrade of every node's agent.
//
// auto_update: off | notify (tell admins about a new release) | panel
// (also install it at auto_update_hour) | all (and then upgrade the agents).

const panelRepo = "cuijianzhuang/polaris"

// updaterUnit is where the root path unit lives (a variable for tests).
var updaterUnit = "/etc/systemd/system/polaris-update.path"

var errUpdaterMissing = errors.New("这台服务器还没有安装自动更新组件：在服务器上执行一次 polaris update（v0.1.17 起会自动安装）；Docker 部署请更新镜像")

func (s *Server) updateRequestPath() string { return filepath.Join(s.cfg.DataDir, "update-request") }
func (s *Server) updateResultPath() string  { return filepath.Join(s.cfg.DataDir, "update-result") }

func (s *Server) updaterInstalled() bool {
	_, err := os.Stat(updaterUnit)
	return err == nil
}

// latest release cache: GitHub is asked at most every 10 minutes.
var latestPanel struct {
	sync.Mutex
	v  string
	at time.Time
}

// latestPanelVersion returns the newest release as "v1.2.3".
func (s *Server) latestPanelVersion(ctx context.Context, fresh bool) (string, error) {
	latestPanel.Lock()
	defer latestPanel.Unlock()
	if !fresh && latestPanel.v != "" && time.Since(latestPanel.at) < 10*time.Minute {
		return latestPanel.v, nil
	}
	v, err := s.latestRelease(ctx, panelRepo, "v")
	if err != nil {
		return "", err
	}
	latestPanel.v, latestPanel.at = "v"+v, time.Now()
	return latestPanel.v, nil
}

// newerThanRunning reports whether v is a newer release than this build.
func newerThanRunning(v string) bool {
	return Version != "dev" && v != "" && versionLess(Version, v)
}

// requestPanelUpdate asks the root updater to install target ("" = latest).
// thenAgents also upgrades every node's agent once the new panel is up.
func (s *Server) requestPanelUpdate(target, who string, thenAgents bool) error {
	if Version == "dev" {
		return errors.New("开发版本不能在线升级")
	}
	if !s.updaterInstalled() {
		return errUpdaterMissing
	}
	if _, err := os.Stat(s.updateRequestPath()); err == nil {
		return errors.New("升级已在进行中，请稍候")
	}
	if target == "" {
		target = "latest"
	}
	s.st.SetSetting("update_then_agents", strconv.FormatBool(thenAgents))
	if err := core.AtomicWrite(s.updateRequestPath(), []byte(target+"\n"), 0o644); err != nil {
		return fmt.Errorf("写入升级请求失败：%w", err)
	}
	s.audit(who, "panel.update_request", target, map[bool]string{true: "panel + agents", false: "panel"}[thenAgents], "")
	return nil
}

// Access settings live in /etc/polaris/polaris.env and are applied by the
// root side (`polaris set-path / set-domain / set-port / set-url`, which also
// handles Caddy and the firewall) through the same request file.
var (
	adminPathRe = regexp.MustCompile(`^/([A-Za-z0-9_-]{4,64})?$`)
	domainRe    = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	publicURLRe = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
)

// requestAccessChange validates and queues one access change.
func (s *Server) requestAccessChange(action, value, who string) error {
	value = strings.TrimSpace(value)
	switch action {
	case "set-path":
		value = "/" + strings.Trim(value, "/")
		if !adminPathRe.MatchString(value) {
			return errors.New("路径只能包含字母、数字、- 和 _，长度 4–64；留空或填 / 表示不使用路径")
		}
	case "set-domain":
		value = strings.ToLower(strings.TrimSuffix(value, "."))
		if !domainRe.MatchString(value) {
			return errors.New("域名格式不正确")
		}
	case "set-port":
		if p, err := strconv.Atoi(value); err != nil || p < 1 || p > 65535 {
			return errors.New("端口应为 1–65535")
		}
	case "set-url":
		value = strings.TrimRight(value, "/")
		if !publicURLRe.MatchString(value) {
			return errors.New("地址格式应为 http(s)://主机[:端口]")
		}
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if !s.updaterInstalled() {
		return errUpdaterMissing
	}
	if _, err := os.Stat(s.updateRequestPath()); err == nil {
		return errors.New("上一项更改还在执行，请稍候")
	}
	if err := core.AtomicWrite(s.updateRequestPath(), []byte(action+" "+value+"\n"), 0o644); err != nil {
		return fmt.Errorf("写入请求失败：%w", err)
	}
	s.audit(who, "panel."+action, value, "", "")
	return nil
}

var accessText = map[string]string{"set-path": "后台路径", "set-domain": "面板域名", "set-port": "面板端口", "set-url": "公开地址"}

type updateResult struct {
	OK          bool
	Action, Arg string // "update" (or empty, from older tools) or set-path / set-domain / set-port / set-url
	From, To    string
	Log         string
}

func parseUpdateResult(b []byte) updateResult {
	var r updateResult
	var log []string
	inLog := false
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := sc.Text()
		if inLog {
			if strings.TrimSpace(line) != "" {
				log = append(log, strings.TrimSpace(line))
			}
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "ok":
			r.OK = v == "1"
		case "from":
			r.From = v
		case "to":
			r.To = v
		case "action":
			r.Action = v
		case "arg":
			r.Arg = v
		default:
			inLog = line == "log:"
		}
	}
	r.Log = strings.Join(log, "\n")
	return r
}

// checkUpdateResult reports a finished update (polled, and at startup).
func (s *Server) checkUpdateResult() {
	b, err := os.ReadFile(s.updateResultPath())
	if err != nil {
		return
	}
	os.Remove(s.updateResultPath())
	r := parseUpdateResult(b)
	if label, ok := accessText[r.Action]; ok {
		last, _ := json.Marshal(map[string]any{"action": r.Action, "value": r.Arg, "ok": r.OK, "log": r.Log, "at": time.Now()})
		s.st.SetSetting("access_last", string(last))
		if r.OK {
			s.audit("system", "panel."+r.Action+"_done", r.Arg, "", "")
			s.notify(Event{Kind: "panel_update", Text: "✅ " + label + "已改为 " + r.Arg})
		} else {
			s.notify(Event{Kind: "panel_update", Text: "❌ 修改" + label + "失败：" + r.Arg + "\n" + truncate(r.Log, 800)})
		}
		return
	}
	switch {
	case !r.OK:
		s.st.SetSetting("auto_update_failed", strings.TrimPrefix(r.To, "v"))
		s.audit("system", "panel.update_failed", r.From, r.Log, "")
		s.notify(Event{Kind: "panel_update", Text: "❌ 面板升级失败（仍为 " + orDefault(r.From, Version) + "）\n" + truncate(r.Log, 800)})
		return
	case r.From == r.To:
		s.notify(Event{Kind: "panel_update", Text: "✅ 面板已是最新版本 " + r.To})
	default:
		s.audit("system", "panel.updated", r.From+" → "+r.To, "", "")
		s.notify(Event{Kind: "panel_update", Text: "✅ 面板已升级：" + r.From + " → " + r.To})
	}
	if s.st.GetSetting("update_then_agents", "false") == "true" {
		s.st.SetSetting("update_then_agents", "false")
		go s.upgradeAgentsAfterUpdate()
	}
}

// upgradeAgentsAfterUpdate waits for agents to reconnect to the new panel,
// then upgrades the outdated ones and reports.
func (s *Server) upgradeAgentsAfterUpdate() {
	select {
	case <-time.After(30 * time.Second):
	case <-s.ctxBG().Done():
		return
	}
	ctx, cancel := context.WithTimeout(s.ctxBG(), 10*time.Minute)
	defer cancel()
	res := s.upgradeOutdatedAgents(ctx)
	if len(res) == 0 {
		s.notify(Event{Kind: "panel_update", Text: "✅ 所有在线节点的 Agent 已是 " + Version})
		return
	}
	s.notify(Event{Kind: "panel_update", Text: agentUpgradeSummary(res)})
}

type agentUpgradeResult struct {
	Node  string `json:"node"`
	Ok    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func agentUpgradeSummary(res []agentUpgradeResult) string {
	ok := 0
	var failed []string
	for _, r := range res {
		if r.Ok {
			ok++
		} else {
			failed = append(failed, r.Node+"："+r.Error)
		}
	}
	text := fmt.Sprintf("⬆️ 节点 Agent 升级到 %s：成功 %d 个", Version, ok)
	if len(failed) > 0 {
		text += fmt.Sprintf("，失败 %d 个\n%s", len(failed), truncate(strings.Join(failed, "\n"), 600))
	}
	return text
}

// autoUpdateTick runs hourly: announces a new release once and, in the
// panel / all modes, installs it during auto_update_hour.
func (s *Server) autoUpdateTick() {
	mode := s.st.GetSetting("auto_update", "notify")
	if mode == "off" || Version == "dev" {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctxBG(), time.Minute)
	defer cancel()
	latest, err := s.latestPanelVersion(ctx, false)
	if err != nil || !newerThanRunning(latest) {
		return
	}
	if s.st.GetSetting("update_notified", "") != latest {
		s.st.SetSetting("update_notified", latest)
		hint := "在机器人里发送 /update 或在面板「设置 → 版本与更新」中升级"
		if mode == "panel" || mode == "all" {
			hint = fmt.Sprintf("将在 %s 点自动升级", s.st.GetSetting("auto_update_hour", "4"))
		}
		s.notify(Event{Kind: "panel_update", Text: fmt.Sprintf("🆕 发现新版本 %s（当前 %s）\n%s\nhttps://github.com/%s/releases/tag/%s", latest, Version, hint, panelRepo, latest)})
	}
	if mode != "panel" && mode != "all" {
		return
	}
	if time.Now().Hour() != atoiDefault(s.st.GetSetting("auto_update_hour", "4"), 4) {
		return
	}
	if s.st.GetSetting("auto_update_failed", "") == strings.TrimPrefix(latest, "v") {
		return // failed once: leave this release to a manual update
	}
	if err := s.requestPanelUpdate(latest, "auto-update", mode == "all"); err != nil {
		s.log.Warn("auto update", "err", err)
		return
	}
	s.notify(Event{Kind: "panel_update", Text: "⏳ 开始自动升级到 " + latest})
}

// --- API ---

// getAccess is GET /system/access: how the panel is reached.
func (s *Server) getAccess(c *gin.Context) {
	out := gin.H{"adminPath": s.cfg.AdminPath, "publicUrl": s.cfg.PublicURL, "listen": s.cfg.Listen, "updater": s.updaterInstalled()}
	if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Scheme == "https" {
		out["domain"] = u.Hostname()
	}
	if _, err := os.Stat(s.updateRequestPath()); err == nil {
		out["pending"] = true
	}
	if v := s.st.GetSetting("access_last", ""); v != "" {
		out["last"] = json.RawMessage(v)
	}
	c.JSON(http.StatusOK, out)
}

// postAccess is POST /system/access {action, value}.
func (s *Server) postAccess(c *gin.Context) {
	var req struct {
		Action string `json:"action" binding:"required"`
		Value  string `json:"value"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.requestAccessChange(req.Action, req.Value, c.GetString("admin")); err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

// getPanelUpdate is GET /system/update.
func (s *Server) getPanelUpdate(c *gin.Context) {
	out := gin.H{"current": Version, "updater": s.updaterInstalled()}
	if _, err := os.Stat(s.updateRequestPath()); err == nil {
		out["pending"] = true
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	if v, err := s.latestPanelVersion(ctx, c.Query("fresh") == "1"); err == nil {
		out["latest"], out["newer"] = v, newerThanRunning(v)
	} else {
		out["error"] = err.Error()
	}
	c.JSON(http.StatusOK, out)
}

// postPanelUpdate is POST /system/update {agents}.
func (s *Server) postPanelUpdate(c *gin.Context) {
	var req struct {
		Agents bool `json:"agents"`
	}
	_ = c.ShouldBindJSON(&req)
	if err := s.requestPanelUpdate("", c.GetString("admin"), req.Agents); err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

// upgradeOutdatedAgents upgrades every online node running another version.
func (s *Server) upgradeOutdatedAgents(ctx context.Context) []agentUpgradeResult {
	var nodes []store.Node
	s.st.DB.Find(&nodes)
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = []agentUpgradeResult{}
		sem = make(chan struct{}, 4)
	)
	for i := range nodes {
		n := &nodes[i]
		if !s.hub.Online(n.ID) || n.AgentVersion == Version {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := s.upgradeAgent(ctx, n)
			r := agentUpgradeResult{Node: n.Name, Ok: err == nil}
			if err != nil {
				r.Error = err.Error()
			}
			mu.Lock()
			out = append(out, r)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}
