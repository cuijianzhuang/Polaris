package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// fakeGitHub serves release assets and the releases API for both
// github.com and api.github.com, keyed by "<repo>/<tag>".
type fakeGitHub struct {
	mu     sync.Mutex
	assets map[string][]byte // "<repo>/<tag>/<asset>" -> body
}

func (f *fakeGitHub) add(repo, tag, asset string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assets[repo+"/"+tag+"/"+asset] = body
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Host == "api.github.com" { // /repos/<o>/<r>/releases/tags/<tag>
		p := strings.TrimPrefix(r.URL.Path, "/repos/")
		repo, tag, _ := strings.Cut(p, "/releases/tags/")
		type asset struct{ Name, Digest string }
		var as []asset
		for k, b := range f.assets {
			if strings.HasPrefix(k, repo+"/"+tag+"/") {
				sum := sha256.Sum256(b)
				as = append(as, asset{strings.TrimPrefix(k, repo+"/"+tag+"/"), "sha256:" + hex.EncodeToString(sum[:])})
			}
		}
		if len(as) == 0 {
			http.NotFound(w, r)
			return
		}
		out := map[string]any{"assets": []map[string]string{}}
		for _, a := range as {
			out["assets"] = append(out["assets"].([]map[string]string), map[string]string{"name": a.Name, "digest": a.Digest})
		}
		json.NewEncoder(w).Encode(out)
		return
	}
	// /<o>/<r>/releases/download/<tag>/<asset>
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 5)
	if len(parts) == 5 {
		repo, rest := parts[0]+"/"+parts[1], parts[4]
		i := strings.LastIndex(rest, "/")
		if b, ok := f.assets[repo+"/"+rest[:i]+"/"+rest[i+1:]]; ok {
			w.Write(b)
			return
		}
	}
	http.NotFound(w, r)
}

func singboxTarball(t *testing.T, v, body string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	name := fmt.Sprintf("sing-box-%s-linux-%s/sing-box", v, runtime.GOARCH)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestSingboxStatsBuild(t *testing.T) {
	gh := &fakeGitHub{assets: map[string][]byte{}}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Host = r.URL.Host // keep github.com vs api.github.com for the fake
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(r)
	})}
	v := "1.13.5"
	asset := fmt.Sprintf("sing-box-%s-linux-%s.tar.gz", v, runtime.GOARCH)
	gh.add("SagerNet/sing-box", "v"+v, asset, singboxTarball(t, v, "upstream"))
	read := func(i *Installer) (string, bool) {
		b, _ := os.ReadFile(i.BinPath(model.CoreSingbox, v))
		_, err := os.Stat(filepath.Join(filepath.Dir(i.BinPath(model.CoreSingbox, v)), statsMarker))
		return string(b), err == nil
	}
	cs := model.CoreSpec{Type: model.CoreSingbox, Version: v}

	// No stats build published: upstream is installed.
	i := &Installer{Dir: t.TempDir(), HTTP: client}
	if _, err := i.Ensure(t.Context(), cs); err != nil {
		t.Fatal(err)
	}
	if body, marked := read(i); body != "upstream" || marked {
		t.Fatalf("fallback install: %q marked=%v", body, marked)
	}
	// The stats build appears: the next agent run swaps it in.
	gh.add(StatsBuildRepo, "sing-box-v"+v, asset, singboxTarball(t, v, "stats"))
	i2 := &Installer{Dir: i.Dir, HTTP: client}
	if _, err := i2.Ensure(t.Context(), cs); err != nil {
		t.Fatal(err)
	}
	if body, marked := read(i2); body != "stats" || !marked {
		t.Fatalf("swap: %q marked=%v", body, marked)
	}
	// Fresh install prefers the stats build.
	i3 := &Installer{Dir: t.TempDir(), HTTP: client}
	if _, err := i3.Ensure(t.Context(), cs); err != nil {
		t.Fatal(err)
	}
	if body, marked := read(i3); body != "stats" || !marked {
		t.Fatalf("fresh: %q marked=%v", body, marked)
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// GitHub unreachable from the node: the install goes through the panel relay.
func TestInstallViaPanelRelay(t *testing.T) {
	gh := &fakeGitHub{assets: map[string][]byte{}}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	v := "1.13.5"
	asset := fmt.Sprintf("sing-box-%s-linux-%s.tar.gz", v, runtime.GOARCH)
	gh.add("SagerNet/sing-box", "v"+v, asset, singboxTarball(t, v, "relayed"))

	blocked := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial tcp: connection timed out")
	})}
	var relayed []string
	i := &Installer{Dir: t.TempDir(), HTTP: blocked, Relay: func(ctx context.Context, u string) (*http.Response, error) {
		relayed = append(relayed, u)
		p, _ := url.Parse(u)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+p.Path, nil)
		req.Host = p.Host
		req.URL.Host = target.Host
		return http.DefaultTransport.RoundTrip(req)
	}}
	cs := model.CoreSpec{Type: model.CoreSingbox, Version: v}
	if _, err := i.Ensure(t.Context(), cs); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(i.BinPath(model.CoreSingbox, v)); string(b) != "relayed" {
		t.Fatalf("binary = %q", b)
	}
	if len(relayed) == 0 || !i.viaPanel.Load() {
		t.Fatalf("relay not used: %v", relayed)
	}
}
