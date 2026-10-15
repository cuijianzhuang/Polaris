package panel

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Agent distribution: the panel keeps a copy of the agent binaries matching
// its own version under <data>/downloads, so nodes install from the panel
// instead of GitHub (which many node networks reach slowly or not at all).
// Docker images ship the binaries already; binary installs fetch them once
// from the release, verified against its checksums.txt.

const agentRepo = "https://github.com/cuijianzhuang/polaris/releases/download/"

var agentArches = []string{"amd64", "arm64"}

func (s *Server) downloadsDir() string { return filepath.Join(s.cfg.DataDir, "downloads") }

// agentCacheCurrent reports whether downloads/ holds agents for this version.
// Files without a .version marker were put there by hand or by the Docker
// entrypoint and are trusted as-is.
func (s *Server) agentCacheCurrent() bool {
	for _, a := range agentArches {
		if _, err := os.Stat(filepath.Join(s.downloadsDir(), "polaris-agent-linux-"+a)); err != nil {
			return false
		}
	}
	v, err := os.ReadFile(filepath.Join(s.downloadsDir(), ".version"))
	return err != nil || strings.TrimSpace(string(v)) == Version
}

// syncAgentBinaries fills downloads/ from the GitHub release of this version.
func (s *Server) syncAgentBinaries(ctx context.Context) error {
	if Version == "dev" || s.agentCacheCurrent() {
		return nil
	}
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
	want := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(sums.String()))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 {
			want[strings.TrimPrefix(f[1], "*")] = f[0]
		}
	}
	if err := os.MkdirAll(s.downloadsDir(), 0o755); err != nil {
		return err
	}
	for _, a := range agentArches {
		name := "polaris-agent-linux-" + a
		if want[name] == "" {
			return fmt.Errorf("checksums.txt has no %s", name)
		}
		tmp, err := os.CreateTemp(s.downloadsDir(), ".dl-*")
		if err != nil {
			return err
		}
		h := sha256.New()
		err = get(base+name, io.MultiWriter(tmp, h), 200<<20)
		tmp.Close()
		if err == nil && hex.EncodeToString(h.Sum(nil)) != want[name] {
			err = fmt.Errorf("%s: checksum mismatch", name)
		}
		if err == nil {
			os.Chmod(tmp.Name(), 0o755)
			err = os.Rename(tmp.Name(), filepath.Join(s.downloadsDir(), name))
		}
		if err != nil {
			os.Remove(tmp.Name())
			return err
		}
	}
	return os.WriteFile(filepath.Join(s.downloadsDir(), ".version"), []byte(Version+"\n"), 0o644)
}

// runAgentSync retries hourly until the cache is filled.
func (s *Server) runAgentSync(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := s.syncAgentBinaries(ctx); err != nil {
			s.log.Warn("could not cache agent binaries; nodes will download them from GitHub", "err", err)
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
