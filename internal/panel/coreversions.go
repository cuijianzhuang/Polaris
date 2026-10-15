package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Core upgrades and rollbacks. The agent keeps every installed version and
// switches a "current" link, and reverts by itself when a new version fails
// to start; the panel then sets the desired version back so it does not
// retry, and offers one-click rollback to the previous version.

// setCoreVersion installs or switches a core on a node (shared by the core
// form, rollback and batch upgrade). It returns an HTTP status on error.
func (s *Server) setCoreVersion(nodeID string, typ model.CoreType, version string, isDefault bool) (*store.Core, int, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("version is required")
	}
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", nodeID).Error; err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("node not found")
	}
	caps, ok := core.Capabilities(typ, version)
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("unknown core %s", typ)
	}
	// Every inbound already on this core must remain supported by the new version.
	var ins []store.Inbound
	s.st.DB.Where("node_id = ? AND core = ?", nodeID, typ).Find(&ins)
	for _, in := range ins {
		m := s.toModelInbound(&in)
		if err := caps.SupportsInbound(&m); err != nil {
			return nil, http.StatusConflict, fmt.Errorf("入站 %s 在该版本不受支持：%v", in.Tag, err)
		}
	}
	var cur store.Core
	err := s.st.DB.Where("node_id = ? AND type = ?", nodeID, typ).First(&cur).Error
	if store.IsNotFound(err) {
		cur = store.Core{ID: store.NewID(), NodeID: nodeID, Type: typ, Version: version, Status: "installing",
			InstalledVersions: store.NewJSON([]string{})}
		err = s.st.DB.Create(&cur).Error
	} else if err == nil && cur.Version != version {
		cur.PrevVersion, cur.Version = cur.Version, version
		err = s.st.DB.Model(&cur).Updates(map[string]any{"version": version, "prev_version": cur.PrevVersion, "status": "installing", "error": ""}).Error
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if isDefault {
		s.st.DB.Model(&store.Core{}).Where("node_id = ?", nodeID).Update("is_default", gorm.Expr("type = ?", typ))
		s.st.DB.Model(&n).Update("default_core", typ)
	}
	s.MarkDirty("core "+string(typ)+" "+version, nodeID)
	return &cur, http.StatusOK, nil
}

// revertFailedUpgrades runs when a node fails to apply a spec. When the
// failure is a core version switch (install failed, or the agent rolled
// back), make the version that is still running desired again so the panel
// stops retrying, and keep the failure visible on the core.
func (s *Server) revertFailedUpgrades(nodeID, reason string) {
	var cs []store.Core
	s.st.DB.Where("node_id = ?", nodeID).Find(&cs)
	for _, c := range cs {
		if c.RunningVersion == "" || c.RunningVersion == c.Version {
			continue
		}
		// Only failures of the switch itself (the agent names the version);
		// an unrelated error such as a port conflict keeps the choice.
		if !strings.Contains(reason, "升级到 "+c.Version) && !strings.Contains(reason, "安装 "+c.Version) {
			continue
		}
		msg := truncate(fmt.Sprintf("升级到 %s 失败，仍运行 %s：%s", c.Version, c.RunningVersion, reason), 1900)
		s.st.DB.Model(&store.Core{}).Where("id = ?", c.ID).Updates(map[string]any{
			"version": c.RunningVersion, "prev_version": c.Version, "error": msg})
		s.audit("agent", "core.upgrade_failed", nodeID, string(c.Type)+" "+c.Version+" → "+c.RunningVersion, "")
		s.MarkDirty("core "+string(c.Type)+" kept at "+c.RunningVersion, nodeID)
	}
}

// rollbackCore switches a core back to its previous (or a chosen installed) version.
func (s *Server) rollbackCore(c *gin.Context) {
	var cur store.Core
	if err := s.st.DB.First(&cur, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req struct {
		Version string `json:"version"`
	}
	_ = c.ShouldBindJSON(&req)
	target := req.Version
	if target == "" {
		target = cur.PrevVersion
	}
	if target == "" {
		for _, v := range cur.InstalledVersions.V {
			if v != cur.Version && (target == "" || versionLess(target, v)) && versionLess(v, cur.Version) {
				target = v
			}
		}
	}
	if target == "" || target == cur.Version {
		badRequest(c, fmt.Errorf("没有可回退的版本"))
		return
	}
	out, code, err := s.setCoreVersion(cur.NodeID, cur.Type, target, false)
	if err != nil {
		c.JSON(code, gin.H{"error": err.Error()})
		return
	}
	s.auditCtx(c, "core.rollback", cur.NodeID, fmt.Sprintf("%s %s → %s", cur.Type, cur.Version, target))
	c.JSON(http.StatusOK, out)
}

// upgradeCores sets one core type to a version on many nodes.
func (s *Server) upgradeCores(c *gin.Context) {
	var req struct {
		Type    model.CoreType `json:"type" binding:"required"`
		Version string         `json:"version" binding:"required"`
		NodeIDs []string       `json:"nodeIds"` // empty = every node that has this core
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	q := s.st.DB.Where("type = ?", req.Type)
	if len(req.NodeIDs) > 0 {
		q = q.Where("node_id IN ?", req.NodeIDs)
	}
	var cs []store.Core
	q.Find(&cs)
	type result struct {
		NodeID string `json:"nodeId"`
		Ok     bool   `json:"ok"`
		Error  string `json:"error,omitempty"`
	}
	out := []result{}
	for _, cur := range cs {
		r := result{NodeID: cur.NodeID, Ok: true}
		if _, _, err := s.setCoreVersion(cur.NodeID, req.Type, req.Version, false); err != nil {
			r.Ok, r.Error = false, err.Error()
		}
		out = append(out, r)
	}
	s.auditCtx(c, "core.upgrade_batch", string(req.Type), fmt.Sprintf("%s on %d nodes", req.Version, len(out)))
	c.JSON(http.StatusOK, gin.H{"results": out})
}

// --- latest releases ---

var coreRepos = map[model.CoreType]struct{ repo, tagPrefix string }{
	model.CoreXray: {"XTLS/Xray-core", "v"}, model.CoreSingbox: {"SagerNet/sing-box", "v"},
	model.CoreHysteria: {"apernet/hysteria", "app/v"}, model.CoreMihomo: {"MetaCubeX/mihomo", "v"},
	model.CoreCaddy: {"caddyserver/caddy", "v"}, model.CoreForwarder: {"zhboner/realm", "v"},
}

type latestCache struct {
	mu   sync.Mutex
	at   time.Time
	vers map[model.CoreType]string
}

var latestVersions latestCache

// coreLatest reports the newest stable GitHub release of each core (cached
// for an hour; a core whose lookup fails is simply missing).
func (s *Server) coreLatest(c *gin.Context) {
	latestVersions.mu.Lock()
	defer latestVersions.mu.Unlock()
	if latestVersions.vers == nil || time.Since(latestVersions.at) > time.Hour || c.Query("refresh") == "1" {
		vers := map[model.CoreType]string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for t, r := range coreRepos {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if v, err := s.latestRelease(c.Request.Context(), r.repo, r.tagPrefix); err == nil {
					mu.Lock()
					vers[t] = v
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		latestVersions.vers, latestVersions.at = vers, time.Now()
	}
	c.JSON(http.StatusOK, latestVersions.vers)
}

// fallbackVersions are used when a default is "latest" but GitHub (and the
// mirror) cannot be reached.
var fallbackVersions = map[model.CoreType]string{
	model.CoreXray: "26.3.27", model.CoreSingbox: "1.13.5", model.CoreHysteria: "2.6.1",
	model.CoreForwarder: "2.7.0", model.CoreMihomo: "1.19.15", model.CoreCaddy: "2.10.2",
}

var coreDefaultKeys = map[model.CoreType]string{model.CoreXray: "default_xray", model.CoreSingbox: "default_singbox"}

// defaultCoreVersion is the version new cores get: the configured default,
// or the newest stable release when it is "latest" (or unset).
func (s *Server) defaultCoreVersion(ctx context.Context, t model.CoreType) string {
	v := ""
	if k, ok := coreDefaultKeys[t]; ok {
		v = strings.TrimPrefix(strings.TrimSpace(s.st.GetSetting(k, "latest")), "v")
	}
	if v != "" && v != "latest" {
		return v
	}
	if r, ok := coreRepos[t]; ok {
		if vs := s.cachedVersions(ctx, t, r.repo, r.tagPrefix); len(vs) > 0 {
			return vs[0]
		}
	}
	return fallbackVersions[t]
}

// cachedVersions returns recent stable versions, cached for an hour.
func (s *Server) cachedVersions(ctx context.Context, t model.CoreType, repo, prefix string) []string {
	versionLists.mu.Lock()
	if versionLists.m == nil {
		versionLists.m = map[model.CoreType]versionList{}
	}
	if l, ok := versionLists.m[t]; ok && time.Since(l.at) < time.Hour {
		versionLists.mu.Unlock()
		return l.vers
	}
	versionLists.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	vs, err := s.releaseVersions(ctx, repo, prefix)
	versionLists.mu.Lock()
	defer versionLists.mu.Unlock()
	if err != nil {
		// Remember the failure for a few minutes so node creation does
		// not wait on an unreachable GitHub every time.
		versionLists.m[t] = versionList{time.Now().Add(-55 * time.Minute), nil}
		return nil
	}
	if len(vs) > 15 {
		vs = vs[:15]
	}
	versionLists.m[t] = versionList{time.Now(), vs}
	return vs
}

// coreDefaults is GET /core-defaults: the version each core type gets when
// added without one (resolving "latest").
func (s *Server) coreDefaults(c *gin.Context) {
	out := map[model.CoreType]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for t := range coreRepos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := s.defaultCoreVersion(c.Request.Context(), t)
			mu.Lock()
			out[t] = v
			mu.Unlock()
		}()
	}
	wg.Wait()
	c.JSON(http.StatusOK, out)
}

var versionLists struct {
	mu sync.Mutex
	m  map[model.CoreType]versionList
}

type versionList struct {
	at   time.Time
	vers []string
}

// coreVersions is GET /core-versions?type=xray: recent stable releases,
// newest first, for the version picker (cached for an hour).
func (s *Server) coreVersions(c *gin.Context) {
	t := model.CoreType(c.Query("type"))
	r, ok := coreRepos[t]
	if !ok {
		badRequest(c, fmt.Errorf("unknown core type %q", t))
		return
	}
	versionLists.mu.Lock()
	defer versionLists.mu.Unlock()
	if versionLists.m == nil {
		versionLists.m = map[model.CoreType]versionList{}
	}
	if l, ok := versionLists.m[t]; ok && l.vers != nil && time.Since(l.at) < time.Hour && c.Query("refresh") != "1" {
		c.JSON(http.StatusOK, gin.H{"versions": l.vers})
		return
	}
	vs, err := s.releaseVersions(c.Request.Context(), r.repo, r.tagPrefix)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "无法获取版本列表：" + err.Error(), "versions": []string{}})
		return
	}
	if len(vs) > 15 {
		vs = vs[:15]
	}
	versionLists.m[t] = versionList{time.Now(), vs}
	c.JSON(http.StatusOK, gin.H{"versions": vs})
}

// GitHub endpoints (variables so tests can point them at a fake).
var (
	githubAPI = "https://api.github.com"
	githubWeb = "https://github.com"
)

// latestRelease finds the newest stable release: from the GitHub API, or,
// when that fails (rate limit, blocked network), from the redirect of
// github.com/<repo>/releases/latest, through github_mirror when set.
func (s *Server) latestRelease(ctx context.Context, repo, prefix string) (string, error) {
	vs, err := s.releaseVersions(ctx, repo, prefix)
	if err != nil {
		return "", err
	}
	return vs[0], nil
}

// flagsEveryRelease lists repositories that mark every build as a GitHub
// "pre-release" (Xray since 26.7): there, only the tag tells a test build
// ("-beta", "+…") from a regular one.
var flagsEveryRelease = map[string]bool{"XTLS/Xray-core": true}

// releaseVersions lists recent stable versions, newest first: from the
// GitHub API, else the releases Atom feed, else the "latest" redirect (all
// through github_mirror when the API is unreachable).
func (s *Server) releaseVersions(ctx context.Context, repo, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	vs, err := versionsFromAPI(ctx, repo, prefix)
	if err == nil {
		return vs, nil
	}
	web := githubWeb
	if s.st != nil {
		if m := strings.TrimRight(s.st.GetSetting("github_mirror", ""), "/"); m != "" {
			web = m + "/" + githubWeb
		}
	}
	if vs, err2 := versionsFromAtom(ctx, web, repo, prefix); err2 == nil {
		return vs, nil
	}
	if v2, err2 := latestFromRedirect(ctx, web, repo, prefix); err2 == nil {
		return []string{v2}, nil
	}
	return nil, err
}

// sortVersions dedupes and orders versions newest first.
func sortVersions(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[j], out[i]) })
	return out
}

var atomTag = regexp.MustCompile(`/releases/tag/([^"<>\s]+)`)

func versionsFromAtom(ctx context.Context, web, repo, prefix string) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, web+"/"+repo+"/releases.atom", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("releases feed: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var vs []string
	for _, m := range atomTag.FindAllStringSubmatch(string(b), -1) {
		tag, _ := url.PathUnescape(m[1])
		if v, ok := stableVersion(tag, prefix); ok {
			vs = append(vs, v)
		}
	}
	if len(vs) == 0 {
		return nil, fmt.Errorf("no stable release in feed")
	}
	return sortVersions(vs), nil
}

func versionsFromAPI(ctx context.Context, repo, prefix string) ([]string, error) {
	// Hysteria tags more than the app ("app/v2.6.1"), so scan recent releases.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+"/repos/"+repo+"/releases?per_page=30", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: %s", resp.Status)
	}
	var rels []struct {
		Tag        string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, err
	}
	var vs []string
	for _, r := range rels {
		if r.Draft || (r.Prerelease && !flagsEveryRelease[repo]) {
			continue
		}
		if v, ok := stableVersion(r.Tag, prefix); ok {
			vs = append(vs, v)
		}
	}
	if len(vs) == 0 {
		return nil, fmt.Errorf("no stable release")
	}
	return sortVersions(vs), nil
}

func latestFromRedirect(ctx context.Context, web, repo, prefix string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, web+"/"+repo+"/releases/latest", nil)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location") // …/releases/tag/<tag>
	i := strings.Index(loc, "/releases/tag/")
	if i < 0 {
		return "", fmt.Errorf("no release redirect (%s)", resp.Status)
	}
	tag, _ := url.PathUnescape(loc[i+len("/releases/tag/"):])
	if v, ok := stableVersion(tag, prefix); ok {
		return v, nil
	}
	return "", fmt.Errorf("latest release %q is not a stable %s* tag", tag, prefix)
}

// stableVersion strips the tag prefix and rejects pre-release builds.
func stableVersion(tag, prefix string) (string, bool) {
	if !strings.HasPrefix(tag, prefix) {
		return "", false
	}
	v := strings.TrimPrefix(tag, prefix)
	if v == "" || strings.ContainsAny(v, "-+") {
		return "", false
	}
	return v, true
}

// versionLess compares dotted numeric versions ("1.13.5" < "1.14.0").
func versionLess(a, b string) bool {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			fmt.Sscanf(pa[i], "%d", &x)
		}
		if i < len(pb) {
			fmt.Sscanf(pb[i], "%d", &y)
		}
		if x != y {
			return x < y
		}
	}
	return false
}
