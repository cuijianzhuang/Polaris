package caddy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func (a *Adapter) renderWebsite(b *strings.Builder, w model.WebsiteSpec, fronts []model.FrontRule) error {
	if w.Port < 1 || w.Port > 65535 || !model.ValidWebsitePath(w.Path) {
		return fmt.Errorf("invalid website listener or path")
	}
	paths := []string{}
	for _, f := range fronts {
		paths = append(paths, "/"+strings.Trim(f.Path, "/"))
	}
	proxies := append([]model.WebsiteProxy(nil), w.Proxies...)
	for _, p := range proxies {
		if !model.ValidWebsitePath(p.Path) || p.Path == "/" {
			return fmt.Errorf("invalid panel path")
		}
		if err := model.ValidateWebsiteUpstream(p.Upstream, w.Port); err != nil {
			return err
		}
		paths = append(paths, p.Path)
	}
	for i, p := range paths {
		for _, other := range paths[:i] {
			if p == other || strings.HasPrefix(p, other+"/") || strings.HasPrefix(other, p+"/") {
				return fmt.Errorf("overlapping website routes: %s / %s", p, other)
			}
		}
	}
	if w.Path != "/" {
		for _, p := range paths {
			if p == w.Path || strings.HasPrefix(p, w.Path+"/") || strings.HasPrefix(w.Path, p+"/") {
				return fmt.Errorf("website path overlaps route %s", p)
			}
		}
	}
	sort.Slice(proxies, func(i, j int) bool { return len(proxies[i].Path) > len(proxies[j].Path) })
	for i, p := range proxies {
		fmt.Fprintf(b, "\t@panel%d path %s %s/*\n\thandle @panel%d {\n\t\treverse_proxy %s {\n\t\t\theader_up -CF-Connecting-IP\n\t\t\theader_up X-Forwarded-For {http.vars.client_ip}\n\t\t}\n\t}\n", i, p.Path, p.Path, i, p.Upstream)
	}
	if w.Path == "/" {
		b.WriteString("\thandle {\n")
	} else {
		fmt.Fprintf(b, "\t@websiteRoot path %s\n\thandle @websiteRoot {\n\t\tredir %s/ 308\n\t}\n\t@website path %s/*\n\thandle @website {\n\t\turi strip_prefix %s\n", w.Path, w.Path, w.Path, w.Path)
	}
	switch w.Mode {
	case "proxy":
		if err := model.ValidateWebsiteUpstream(w.Upstream, w.Port); err != nil {
			return err
		}
		fmt.Fprintf(b, "\t\treverse_proxy %s {\n\t\t\theader_up -CF-Connecting-IP\n\t\t\theader_up X-Forwarded-For {http.vars.client_ip}\n\t\t}\n", w.Upstream)
	case "static":
		root, err := a.writeWebsite(w)
		if err != nil {
			return err
		}
		fmt.Fprintf(b, "\t\troot * %s\n\t\tfile_server\n", strconv.Quote(root))
	default:
		return fmt.Errorf("invalid website mode")
	}
	b.WriteString("\t}\n")
	if w.Path != "/" {
		b.WriteString("\thandle {\n\t\trespond 404\n\t}\n")
	}
	return nil
}

// Immutable, content-addressed trees keep the serving version intact while
// Caddy validates/reloads a new bundle, including a failed-apply rollback.
func (a *Adapter) writeWebsite(w model.WebsiteSpec) (string, error) {
	if err := model.ValidateWebsiteFiles(w.Files); err != nil {
		return "", err
	}
	if _, ok := w.Files["index.html"]; !ok {
		return "", fmt.Errorf("website has no index.html")
	}
	raw, _ := json.Marshal(w.Files)
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	root := filepath.Join(a.opt.WorkDir, "websites", digest)
	if _, err := os.Stat(root); err == nil {
		return root, nil
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp(filepath.Dir(root), ".upload-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	for name, data := range w.Files {
		if err = core.AtomicWrite(filepath.Join(temp, filepath.FromSlash(name)), data, 0o600); err != nil {
			return "", err
		}
	}
	if err = os.Rename(temp, root); err != nil {
		if _, e := os.Stat(root); e != nil {
			return "", err
		}
	}
	return root, nil
}
