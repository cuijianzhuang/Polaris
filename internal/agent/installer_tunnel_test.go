package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

type tunnelReleaseTransport func(*http.Request) (*http.Response, error)

func (f tunnelReleaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCloudflaredInstallerVerifiesOfficialDigest(t *testing.T) {
	rel, err := releaseFor(model.CoreCloudflared, model.CloudflaredVersion)
	if err != nil {
		t.Skip(err)
	}
	binary := []byte("#!/bin/sh\nexit 0\n")
	for _, bad := range []bool{false, true} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(binary))
			if bad {
				digest = "sha256:" + strings.Repeat("0", 64)
			}
			installer := &Installer{Dir: t.TempDir(), HTTP: &http.Client{Transport: tunnelReleaseTransport(func(r *http.Request) (*http.Response, error) {
				body := binary
				if strings.Contains(r.URL.Host, "api.github.com") {
					body, _ = json.Marshal(map[string]any{"assets": []map[string]string{{"name": strings.TrimPrefix(rel.url, rel.url[:strings.LastIndex(rel.url, "/")+1]), "digest": digest}}})
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), ContentLength: int64(len(body)), Header: http.Header{}}, nil
			})}}
			path, err := installer.Ensure(context.Background(), model.CoreSpec{Type: model.CoreCloudflared, Version: model.CloudflaredVersion})
			if bad {
				if err == nil {
					t.Fatal("invalid digest accepted")
				}
				if _, e := os.Stat(installer.BinPath(model.CoreCloudflared, model.CloudflaredVersion)); !os.IsNotExist(e) {
					t.Fatal("unverified binary installed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(path)
			if err != nil || string(contents) != string(binary) {
				t.Fatal("raw cloudflared asset not installed", err)
			}
		})
	}
}
