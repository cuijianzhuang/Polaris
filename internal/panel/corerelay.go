package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Core download relay: nodes that cannot reach GitHub (or reach it too slowly,
// or hit the API rate limit) fetch core releases and their digests through
// the panel instead. Only release assets and release metadata of the core
// repositories are relayed; assets are cached on disk (a release asset does
// not change), API answers in memory for a few minutes. The agent still
// verifies every archive against the official digest.

var relayRepos = map[string]bool{
	"xtls/xray-core": true, "sagernet/sing-box": true, "cuijianzhuang/polaris": true, "apernet/hysteria": true,
	"metacubex/mihomo": true, "caddyserver/caddy": true, "go-gost/gost": true, "zhboner/realm": true,
}

// relayTarget checks a URL the agent asks for and returns the repository
// and whether it is an API call.
func relayTarget(u string) (api bool, ok bool) {
	if strings.Contains(u, "..") || strings.ContainsAny(u, "?#\\ ") {
		return false, false
	}
	var rest string
	switch {
	case strings.HasPrefix(u, "https://github.com/"):
		rest = strings.TrimPrefix(u, "https://github.com/")
		p := strings.SplitN(rest, "/", 5)
		return false, len(p) == 5 && relayRepos[strings.ToLower(p[0]+"/"+p[1])] && p[2] == "releases" && p[3] == "download" && p[4] != ""
	case strings.HasPrefix(u, "https://api.github.com/repos/"):
		rest = strings.TrimPrefix(u, "https://api.github.com/repos/")
		p := strings.SplitN(rest, "/", 5)
		return true, len(p) == 5 && relayRepos[strings.ToLower(p[0]+"/"+p[1])] && p[2] == "releases" && p[3] == "tags" && p[4] != ""
	}
	return false, false
}

var relayAPICache struct {
	mu sync.Mutex
	m  map[string]relayAPIEntry
}

type relayAPIEntry struct {
	body []byte
	at   time.Time
}

var relayClient = &http.Client{Timeout: 15 * time.Minute}

// relayUpstreams lists where to fetch a URL from: github.com through the
// configured mirror first (when set), then directly.
func (s *Server) relayUpstreams(u string, api bool) []string {
	if api {
		return []string{githubAPI + strings.TrimPrefix(u, "https://api.github.com")}
	}
	direct := githubWeb + strings.TrimPrefix(u, "https://github.com")
	if m := strings.TrimRight(s.st.GetSetting("github_mirror", ""), "/"); m != "" {
		return []string{m + "/" + u, direct}
	}
	return []string{direct}
}

func relayFetch(r *http.Request, up string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, up, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "polaris-panel")
	resp, err := relayClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s", resp.Status)
	}
	return resp, nil
}

// serveCoreRelay is GET /agent/core?u=<release URL> on the agent endpoint.
func (s *Server) serveCoreRelay(w http.ResponseWriter, r *http.Request) {
	if _, err := (&agentService{s: s}).node(r.Context()); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.relayCore(w, r)
}

func (s *Server) relayCore(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("u")
	api, ok := relayTarget(u)
	if !ok {
		http.Error(w, "not a core release URL", http.StatusBadRequest)
		return
	}
	if api {
		s.relayAPI(w, r, u)
		return
	}
	sum := sha256.Sum256([]byte(u))
	dir := filepath.Join(s.downloadsDir(), "cores")
	p := filepath.Join(dir, hex.EncodeToString(sum[:16])+"-"+filepath.Base(u))
	if _, err := os.Stat(p); err == nil {
		http.ServeFile(w, r, p)
		return
	}
	var resp *http.Response
	var errs []string
	for _, up := range s.relayUpstreams(u, false) {
		var err error
		if resp, err = relayFetch(r, up); err == nil {
			break
		}
		errs = append(errs, err.Error())
	}
	if resp == nil {
		http.Error(w, "主控也无法下载："+strings.Join(errs, "; "), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	// Stream to the node while keeping a copy, so progress shows at once.
	os.MkdirAll(dir, 0o755)
	tmp, terr := os.CreateTemp(dir, ".dl-*")
	if resp.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	var dst io.Writer = w
	if terr == nil {
		dst = io.MultiWriter(w, tmp)
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, 512<<20))
	if terr != nil {
		return
	}
	tmp.Close()
	if err != nil || (resp.ContentLength > 0 && n != resp.ContentLength) {
		os.Remove(tmp.Name())
		return
	}
	if os.Rename(tmp.Name(), p) == nil {
		s.log.Info("cached core release", "url", u)
		pruneRelayCache(dir, 30*24*time.Hour)
	}
}

func (s *Server) relayAPI(w http.ResponseWriter, r *http.Request, u string) {
	relayAPICache.mu.Lock()
	if relayAPICache.m == nil {
		relayAPICache.m = map[string]relayAPIEntry{}
	}
	e, hit := relayAPICache.m[u]
	relayAPICache.mu.Unlock()
	if !hit || time.Since(e.at) > 10*time.Minute {
		resp, err := relayFetch(r, s.relayUpstreams(u, true)[0])
		if err != nil {
			if !hit { // a stale answer beats none (rate limits)
				http.Error(w, "主控也无法访问 GitHub API："+err.Error(), http.StatusBadGateway)
				return
			}
		} else {
			b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			e = relayAPIEntry{body: b, at: time.Now()}
			relayAPICache.mu.Lock()
			relayAPICache.m[u] = e
			relayAPICache.mu.Unlock()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(e.body)
}

// pruneRelayCache drops cached releases not touched for a while.
func pruneRelayCache(dir string, age time.Duration) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > age {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
