package panel

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Agent distribution: the panel keeps a copy of the agent binary matching
// its own version under <data>/downloads, so nodes install from the panel
// instead of GitHub (which many node networks reach slowly or not at all).
// Only the panel's own architecture is fetched up front; another one is
// fetched the first time a node of that architecture asks for it (install
// script, self-upgrade), verified against the release's checksums.txt.
// Docker images ship the binaries already.

const agentRepo = "https://github.com/cuijianzhuang/polaris/releases/download/"

var agentArches = []string{"amd64", "arm64"}

func validArch(a string) bool { return a == "amd64" || a == "arm64" }

func (s *Server) downloadsDir() string { return filepath.Join(s.cfg.DataDir, "downloads") }

func (s *Server) agentPath(arch string) string {
	return filepath.Join(s.downloadsDir(), "polaris-agent-linux-"+arch)
}

// agentCurrent reports whether downloads/ holds the agent for arch at this
// version. Files without a version marker were put there by hand or by the
// Docker entrypoint and are trusted as-is.
func (s *Server) agentCurrent(arch string) bool {
	if _, err := os.Stat(s.agentPath(arch)); err != nil {
		return false
	}
	v, err := os.ReadFile(filepath.Join(s.downloadsDir(), ".version-"+arch))
	if err != nil {
		v, err = os.ReadFile(filepath.Join(s.downloadsDir(), ".version")) // older single marker
	}
	return err != nil || strings.TrimSpace(string(v)) == Version
}

// agentCacheCurrent reports whether the panel's own architecture is cached.
func (s *Server) agentCacheCurrent() bool { return s.agentCurrent(runtime.GOARCH) }

var agentFetchMu sync.Mutex

// ensureAgent makes sure downloads/ holds the agent for arch at this version.
func (s *Server) ensureAgent(ctx context.Context, arch string) error {
	if !validArch(arch) {
		return fmt.Errorf("unsupported architecture %q", arch)
	}
	if s.agentCurrent(arch) {
		return nil
	}
	if Version == "dev" {
		return errors.New("development build: no release agents to fetch")
	}
	agentFetchMu.Lock()
	defer agentFetchMu.Unlock()
	if s.agentCurrent(arch) {
		return nil
	}
	if err := s.fetchRelease(ctx, "polaris-agent-linux-"+arch, s.agentPath(arch), 0o755, 200<<20); err != nil {
		return err
	}
	s.log.Info("cached agent binary", "arch", arch, "version", Version)
	return os.WriteFile(filepath.Join(s.downloadsDir(), ".version-"+arch), []byte(Version+"\n"), 0o644)
}

// fetchRelease downloads one asset of this version's release (through the
// configured GitHub mirror) into dst, verified against checksums.txt.
func (s *Server) fetchRelease(ctx context.Context, name, dst string, mode os.FileMode, limit int64) error {
	base := agentRepo + Version + "/"
	if m := strings.TrimRight(s.st.GetSetting("github_mirror", ""), "/"); m != "" {
		base = m + "/" + base
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	get := func(u string, w io.Writer, limit int64) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", u, resp.Status)
		}
		_, err = io.Copy(w, io.LimitReader(resp.Body, limit))
		return err
	}
	var sums strings.Builder
	if err := get(base+"checksums.txt", &sums, 1<<20); err != nil {
		return err
	}
	want := ""
	sc := bufio.NewScanner(strings.NewReader(sums.String()))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt has no %s", name)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".dl-*")
	if err != nil {
		return err
	}
	h := sha256.New()
	err = get(base+name, io.MultiWriter(tmp, h), limit)
	tmp.Close()
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = fmt.Errorf("%s: checksum mismatch", name)
	}
	if err == nil {
		os.Chmod(tmp.Name(), mode)
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// toolPath is the polaris management script handed to nodes. The panel's
// own script keeps it current on install and update; a panel updated by an
// older script may lack it, so it is fetched from the release on demand.
func (s *Server) toolPath() string { return filepath.Join(s.downloadsDir(), "install-panel.sh") }

var toolFetchMu sync.Mutex

func (s *Server) ensureTool(ctx context.Context) error {
	if _, err := os.Stat(s.toolPath()); err == nil {
		return nil
	}
	if Version == "dev" {
		return errors.New("development build: no release script to fetch")
	}
	toolFetchMu.Lock()
	defer toolFetchMu.Unlock()
	if _, err := os.Stat(s.toolPath()); err == nil {
		return nil
	}
	if err := s.fetchRelease(ctx, "install-panel.sh", s.toolPath(), 0o644, 4<<20); err != nil {
		return err
	}
	s.log.Info("cached polaris management script", "version", Version)
	return nil
}

// serveTool is GET /agent/tool on the agent endpoint (enrolled nodes only).
func (s *Server) serveTool(w http.ResponseWriter, r *http.Request) {
	if _, err := (&agentService{s: s}).node(r.Context()); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := s.ensureTool(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, s.toolPath())
}

// syncAgentBinaries caches the panel's own architecture and drops builds of
// other architectures left from an older version (they would be handed out
// with a stale checksum; they are fetched again on demand).
func (s *Server) syncAgentBinaries(ctx context.Context) error {
	for _, a := range agentArches {
		if a != runtime.GOARCH && !s.agentCurrent(a) {
			os.Remove(s.agentPath(a))
		}
	}
	if Version == "dev" {
		return nil
	}
	return s.ensureAgent(ctx, runtime.GOARCH)
}

// runAgentSync retries hourly until the cache is filled.
func (s *Server) runAgentSync(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := s.syncAgentBinaries(ctx); err != nil {
			s.log.Warn("could not cache agent binary; nodes will download it from GitHub", "err", err)
		}
		if s.agentCacheCurrent() || Version == "dev" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// requestBase is the panel's public base URL: POLARIS_PUBLIC_URL, else the
// host the admin is using right now (validated: it ends up in shell scripts).
func (s *Server) requestBase(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	if r != nil && validHost.MatchString(r.Host) {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		return scheme + "://" + r.Host
	}
	return "http://" + s.cfg.Listen
}

// agentURLFor is the endpoint agents dial; without POLARIS_AGENT_HOSTS or a
// public URL it falls back to the host of the current request rather than
// 127.0.0.1, which never works from a node.
func (s *Server) agentURLFor(r *http.Request) string {
	u := s.agentURL()
	if len(s.cfg.AgentHosts) > 0 || s.cfg.PublicURL != "" || r == nil {
		return u
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" || !validHost.MatchString(host) && net.ParseIP(host) == nil {
		return u
	}
	_, port, err := net.SplitHostPort(s.cfg.AgentListen)
	if err != nil {
		port = "9443"
	}
	return (&url.URL{Scheme: "https", Host: net.JoinHostPort(host, port)}).String()
}
