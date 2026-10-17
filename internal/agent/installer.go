package agent

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Installer downloads core releases into <data>/cores/{type}/{version}/ and
// verifies the official SHA-256 before use. Switching versions only flips the
// "current" symlink, so rollback is instant.
type Installer struct {
	Dir    string
	Mirror string // optional prefix replacing https://github.com
	HTTP   *http.Client
	// AllowUnverified permits installs when no official digest is published.
	AllowUnverified bool
	// progress, when set, receives the install steps and download bytes.
	progress *progressTracker
	// Relay, when set, fetches a GitHub URL through the panel; used when
	// GitHub cannot be reached directly (and from then on for this run).
	Relay func(ctx context.Context, u string) (*http.Response, error)

	viaPanel atomic.Bool

	mu    sync.Mutex
	tried map[string]bool // sing-box versions already offered the stats build this run
}

// StatsBuildRepo publishes sing-box built with v2ray_api (per-user traffic),
// as release "sing-box-v<version>"; see .github/workflows/singbox.yml.
const StatsBuildRepo = "cuijianzhuang/Polaris"

// statsMarker sits next to a sing-box binary installed from StatsBuildRepo.
const statsMarker = ".polaris-stats-build"

type release struct {
	url    string
	binary string // path of the executable inside the archive
	repo   string // owner/name for the GitHub API digest lookup
	tag    string
	digest func(ctx context.Context, i *Installer, asset string) (string, error)
	// fallback is tried when this release cannot be fetched or verified.
	fallback *release
	marker   string // file written next to the binary on success
}

func goarch() string { return runtime.GOARCH }

func releaseFor(t model.CoreType, ver string) (*release, error) {
	v := strings.TrimPrefix(ver, "v")
	arch := goarch()
	switch t {
	case model.CoreXray:
		a := map[string]string{"amd64": "64", "arm64": "arm64-v8a", "386": "32", "arm": "arm32-v7a"}[arch]
		asset := "Xray-linux-" + a + ".zip"
		return &release{
			url: "https://github.com/XTLS/Xray-core/releases/download/v" + v + "/" + asset, binary: "xray",
			repo: "XTLS/Xray-core", tag: "v" + v,
			digest: func(ctx context.Context, i *Installer, _ string) (string, error) {
				// Xray publishes <asset>.dgst with "SHA2-256= <hex>".
				b, err := i.get(ctx, "https://github.com/XTLS/Xray-core/releases/download/v"+v+"/"+asset+".dgst")
				if err != nil {
					return "", err
				}
				sc := bufio.NewScanner(bytes.NewReader(b))
				for sc.Scan() {
					l := sc.Text()
					if strings.HasPrefix(l, "SHA2-256=") || strings.HasPrefix(l, "SHA256=") {
						return strings.TrimSpace(l[strings.Index(l, "=")+1:]), nil
					}
				}
				return "", errors.New("no SHA2-256 in .dgst")
			},
		}, nil
	case model.CoreSingbox:
		// Official builds lack v2ray_api, so prefer Polaris's build of the
		// same version (per-user traffic) and fall back to upstream.
		asset := fmt.Sprintf("sing-box-%s-linux-%s.tar.gz", v, arch)
		inner := fmt.Sprintf("sing-box-%s-linux-%s/sing-box", v, arch)
		upstream := &release{
			url:    "https://github.com/SagerNet/sing-box/releases/download/v" + v + "/" + asset,
			binary: inner, repo: "SagerNet/sing-box", tag: "v" + v, digest: githubDigest,
		}
		return &release{
			url:    "https://github.com/" + StatsBuildRepo + "/releases/download/sing-box-v" + v + "/" + asset,
			binary: inner, repo: StatsBuildRepo, tag: "sing-box-v" + v, digest: githubDigest,
			fallback: upstream, marker: statsMarker,
		}, nil
	case model.CoreHysteria:
		asset := "hysteria-linux-" + arch
		return &release{
			url: "https://github.com/apernet/hysteria/releases/download/app/v" + v + "/" + asset, binary: asset,
			repo: "apernet/hysteria", tag: "app/v" + v,
			digest: func(ctx context.Context, i *Installer, _ string) (string, error) {
				b, err := i.get(ctx, "https://github.com/apernet/hysteria/releases/download/app/v"+v+"/hashes.txt")
				if err != nil {
					return "", err
				}
				for _, l := range strings.Split(string(b), "\n") {
					f := strings.Fields(l)
					if len(f) == 2 && strings.HasSuffix(f[1], asset) {
						return f[0], nil
					}
				}
				return "", errors.New("asset not in hashes.txt")
			},
		}, nil
	case model.CoreMihomo:
		asset := fmt.Sprintf("mihomo-linux-%s-v%s.gz", arch, v)
		return &release{
			url: "https://github.com/MetaCubeX/mihomo/releases/download/v" + v + "/" + asset, binary: "mihomo",
			repo: "MetaCubeX/mihomo", tag: "v" + v, digest: githubDigest,
		}, nil
	case model.CoreCaddy:
		if v == "" {
			v = "2.10.2"
		}
		asset := fmt.Sprintf("caddy_%s_linux_%s.tar.gz", v, arch)
		return &release{
			url: "https://github.com/caddyserver/caddy/releases/download/v" + v + "/" + asset, binary: "caddy",
			repo: "caddyserver/caddy", tag: "v" + v,
			digest: func(ctx context.Context, i *Installer, u string) (string, error) {
				if d, err := githubDigest(ctx, i, u); err == nil {
					return d, nil
				}
				// Caddy publishes caddy_<v>_checksums.txt (SHA-512) — fall back to it.
				return "", errors.New("caddy: no sha256 digest published")
			},
		}, nil
	case model.CoreGost:
		if v == "" {
			v = "3.2.6"
		}
		asset := fmt.Sprintf("gost_%s_linux_%s.tar.gz", v, arch)
		return &release{
			url: "https://github.com/go-gost/gost/releases/download/v" + v + "/" + asset, binary: "gost",
			repo: "go-gost/gost", tag: "v" + v, digest: githubDigest,
		}, nil
	case model.CoreForwarder:
		if v == "" {
			v = "2.7.0"
		}
		a := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch]
		asset := "realm-" + a + "-unknown-linux-gnu.tar.gz"
		return &release{
			url: "https://github.com/zhboner/realm/releases/download/v" + v + "/" + asset, binary: "realm",
			repo: "zhboner/realm", tag: "v" + v, digest: githubDigest,
		}, nil
	}
	return nil, fmt.Errorf("no installer for core %s", t)
}

// githubDigest reads the asset's "digest" field from the GitHub releases API.
func githubDigest(ctx context.Context, i *Installer, assetURL string) (string, error) {
	r, err := releaseByURL(assetURL)
	if err != nil {
		return "", err
	}
	b, err := i.get(ctx, "https://api.github.com/repos/"+r.repo+"/releases/tags/"+r.tag)
	if err != nil {
		return "", err
	}
	var rel struct {
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &rel); err != nil {
		return "", err
	}
	name := filepath.Base(assetURL)
	for _, a := range rel.Assets {
		if a.Name == name && strings.HasPrefix(a.Digest, "sha256:") {
			return strings.TrimPrefix(a.Digest, "sha256:"), nil
		}
	}
	return "", errors.New("release publishes no digest for " + name)
}

type repoTag struct{ repo, tag string }

func releaseByURL(u string) (repoTag, error) {
	// https://github.com/<owner>/<repo>/releases/download/<tag...>/<asset>
	p := strings.TrimPrefix(u, "https://github.com/")
	parts := strings.Split(p, "/")
	if len(parts) < 6 {
		return repoTag{}, errors.New("not a GitHub release URL")
	}
	return repoTag{repo: parts[0] + "/" + parts[1], tag: strings.Join(parts[4:len(parts)-1], "/")}, nil
}

// directClient has no overall deadline (a large core on a slow link takes
// a while); a stalled download is cut by stallTimeout instead.
var directClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
	ForceAttemptHTTP2:     true,
}}

// stallTimeout ends a download that received nothing for this long.
var stallTimeout = 60 * time.Second

func (i *Installer) client() *http.Client {
	if i.HTTP != nil {
		return i.HTTP
	}
	return directClient
}

func (i *Installer) mirror(u string) string {
	if i.Mirror != "" && strings.HasPrefix(u, "https://github.com/") {
		return strings.TrimRight(i.Mirror, "/") + "/" + strings.TrimPrefix(u, "https://github.com/")
	}
	return u
}

func (i *Installer) get(ctx context.Context, u string) ([]byte, error) {
	return i.getP(ctx, u, "")
}

// getP is get with the download reported under stage (when non-empty). It
// goes to GitHub (or the mirror) first and through the panel when that
// fails; after one failure the rest of the run uses the panel directly.
func (i *Installer) getP(ctx context.Context, u, stage string) ([]byte, error) {
	if i.Relay == nil || !i.viaPanel.Load() {
		b, err := i.fetch(ctx, u, stage, false)
		if err == nil || i.Relay == nil || ctx.Err() != nil || errors.Is(err, errNotFound) {
			return b, err // a missing asset is missing through the panel too
		}
		i.viaPanel.Store(true)
		b, rerr := i.fetch(ctx, u, stage, true)
		if rerr != nil {
			return nil, fmt.Errorf("直连失败：%v；经主控下载也失败：%v", err, rerr)
		}
		return b, nil
	}
	b, err := i.fetch(ctx, u, stage, true)
	if err != nil && ctx.Err() == nil {
		// The panel may be the problem this time; try GitHub once more.
		if b2, err2 := i.fetch(ctx, u, stage, false); err2 == nil {
			return b2, nil
		}
		return nil, fmt.Errorf("经主控下载失败：%w", err)
	}
	return b, err
}

func (i *Installer) fetch(ctx context.Context, u, stage string, viaPanel bool) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var resp *http.Response
	var err error
	if viaPanel {
		if stage != "" {
			stage += "（经主控）"
			i.step(stage, 0, 0)
		}
		resp, err = i.Relay(ctx, u)
	} else {
		var req *http.Request
		if req, err = http.NewRequestWithContext(ctx, http.MethodGet, i.mirror(u), nil); err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "polaris-agent")
		resp, err = i.client().Do(req)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		err := fmt.Errorf("GET %s: %s %s", u, resp.Status, strings.TrimSpace(string(msg)))
		if resp.StatusCode == http.StatusNotFound && !viaPanel && i.Mirror == "" {
			err = fmt.Errorf("%w: %w", errNotFound, err)
		}
		return nil, err
	}
	var body io.Reader = &stallReader{r: resp.Body, cancel: cancel}
	if stage != "" && i.progress != nil {
		body = &progressReader{r: body, total: max(resp.ContentLength, 0), stage: stage, p: i.progress}
	}
	b, err := io.ReadAll(io.LimitReader(body, 512<<20))
	if err != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("下载中断（%s 内没有收到数据）", stallTimeout)
	}
	if err == nil && resp.ContentLength > 0 && int64(len(b)) != resp.ContentLength {
		return nil, fmt.Errorf("下载不完整：%d / %d 字节", len(b), resp.ContentLength)
	}
	return b, err
}

// errNotFound: GitHub itself answered 404 (no such release or asset).
var errNotFound = errors.New("not found")

// stallReader cancels the request when no data arrives for stallTimeout.
type stallReader struct {
	r      io.Reader
	cancel context.CancelFunc
	t      *time.Timer
}

func (s *stallReader) Read(b []byte) (int, error) {
	if s.t == nil {
		s.t = time.AfterFunc(stallTimeout, s.cancel)
	}
	s.t.Reset(stallTimeout)
	n, err := s.r.Read(b)
	if err != nil {
		s.t.Stop()
	}
	return n, err
}

// BinPath returns the executable path for an installed version.
func (i *Installer) BinPath(t model.CoreType, ver string) string {
	name := map[model.CoreType]string{model.CoreXray: "xray", model.CoreSingbox: "sing-box", model.CoreHysteria: "hysteria",
		model.CoreForwarder: "realm", model.CoreMihomo: "mihomo", model.CoreCaddy: "caddy", model.CoreGost: "gost"}[t]
	return filepath.Join(i.Dir, string(t), versionDir(ver), name)
}

func versionDir(v string) string {
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return "default"
	}
	return v
}

// Installed lists installed versions of a core.
func (i *Installer) Installed(t model.CoreType) []string {
	ents, _ := os.ReadDir(filepath.Join(i.Dir, string(t)))
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Ensure installs the core version if missing and points "current" at it.
// A sing-box installed from upstream is swapped for the stats build once per
// run when one exists, so per-user traffic starts working on old installs.
func (i *Installer) Ensure(ctx context.Context, cs model.CoreSpec) (string, error) {
	bin := i.BinPath(cs.Type, cs.Version)
	rel, relErr := releaseFor(cs.Type, cs.Version)
	if _, err := os.Stat(bin); err == nil {
		if relErr == nil && rel.marker != "" && i.firstTry(cs) {
			if _, err := os.Stat(filepath.Join(filepath.Dir(bin), rel.marker)); err != nil {
				_ = i.install(ctx, cs, rel, bin) // keep the working binary on failure
			}
		}
		return bin, i.link(cs)
	}
	if relErr != nil {
		return "", relErr
	}
	err := i.install(ctx, cs, rel, bin)
	if err != nil && rel.fallback != nil {
		if ferr := i.install(ctx, cs, rel.fallback, bin); ferr != nil {
			return "", fmt.Errorf("%w; fallback: %v", err, ferr)
		}
		err = nil
	}
	if err != nil {
		return "", err
	}
	return bin, i.link(cs)
}

func (i *Installer) firstTry(cs model.CoreSpec) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.tried == nil {
		i.tried = map[string]bool{}
	}
	k := string(cs.Type) + "@" + cs.Version
	if i.tried[k] {
		return false
	}
	i.tried[k] = true
	return true
}

// install downloads, verifies and unpacks one release into bin's directory.
func (i *Installer) install(ctx context.Context, cs model.CoreSpec, rel *release, bin string) error {
	var err error
	name := coreLabel(cs)
	i.step("获取 "+name+" 校验值", 0, 0)
	want := strings.ToLower(cs.SHA256)
	if want == "" || rel.marker != "" { // a pinned digest is the upstream asset's
		if want, err = rel.digest(ctx, i, rel.url); err != nil && (!i.AllowUnverified || rel.marker != "") {
			return fmt.Errorf("cannot verify %s %s: %w", cs.Type, cs.Version, err)
		}
	}
	data, err := i.getP(ctx, rel.url, "下载 "+name)
	if err != nil {
		return err
	}
	i.step("校验并解压 "+name, 0, 0)
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if want != "" && !strings.EqualFold(got, want) {
		return fmt.Errorf("%s %s: sha256 mismatch (got %s, want %s)", cs.Type, cs.Version, got, want)
	}
	dir := filepath.Dir(bin)
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	if err := extract(rel.url, data, tmp, rel.binary, filepath.Base(bin)); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if rel.marker != "" {
		os.WriteFile(filepath.Join(tmp, rel.marker), []byte(rel.url+"\n"), 0o644)
	}
	// Replace the directory; a running process keeps its open binary.
	old := dir + ".old"
	os.RemoveAll(old)
	if _, err := os.Stat(dir); err == nil {
		if err := os.Rename(dir, old); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.Rename(old, dir)
		return err
	}
	os.RemoveAll(old)
	return nil
}

func (i *Installer) step(stage string, done, total int64) {
	if i.progress != nil {
		i.progress.step(stage, done, total)
	}
}

// coreLabel names a core version for progress messages.
func coreLabel(cs model.CoreSpec) string {
	if cs.Version == "" {
		return string(cs.Type)
	}
	return string(cs.Type) + " " + strings.TrimPrefix(cs.Version, "v")
}

func (i *Installer) link(cs model.CoreSpec) error {
	cur := filepath.Join(i.Dir, string(cs.Type), "current")
	tmp := cur + ".new"
	os.Remove(tmp)
	if err := os.Symlink(versionDir(cs.Version), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, cur)
}

// extract unpacks the release; for archives every regular file is kept (Xray
// ships geoip/geosite data next to the binary) and the executable is renamed.
func extract(url string, data []byte, dst, inner, binName string) error {
	writeFile := func(name string, r io.Reader, mode os.FileMode) error {
		p := filepath.Join(dst, filepath.Base(name))
		if filepath.Clean(name) == filepath.Clean(inner) || filepath.Base(name) == filepath.Base(inner) {
			p = filepath.Join(dst, binName)
			mode = 0o755
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(f, io.LimitReader(r, 512<<20))
		return err
	}
	switch {
	case strings.HasSuffix(url, ".zip"):
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return err
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = writeFile(f.Name, rc, 0o644)
			rc.Close()
			if err != nil {
				return err
			}
		}
	case strings.HasSuffix(url, ".gz") && !strings.HasSuffix(url, ".tar.gz"):
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return err
		}
		return writeFile(inner, gz, 0o755)
	case strings.HasSuffix(url, ".tar.gz"):
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return err
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			if err := writeFile(h.Name, tr, 0o644); err != nil {
				return err
			}
		}
	default:
		return writeFile(inner, bytes.NewReader(data), 0o755)
	}
	if _, err := os.Stat(filepath.Join(dst, binName)); err != nil {
		return fmt.Errorf("executable %s not found in archive", inner)
	}
	return nil
}
