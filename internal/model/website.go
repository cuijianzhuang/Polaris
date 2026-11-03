package model

import (
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// WebsiteFeature prevents old agents from silently ignoring website routes.
const WebsiteFeature = "unified-web-entry-v1"

const MaxWebsiteBytes = 8 << 20
const MaxWebsiteFiles = 2048

func ValidWebsitePath(p string) bool {
	if p == "/" {
		return true
	}
	if !strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, c := range p {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._~-", c) {
			return false
		}
	}
	return len(p) <= 200 && !strings.Contains(p, "//")
}

func ValidateWebsiteFiles(files map[string][]byte) error {
	if len(files) > MaxWebsiteFiles {
		return fmt.Errorf("静态网站最多 %d 个文件", MaxWebsiteFiles)
	}
	size := 0
	for name, data := range files {
		if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00:") || len(name) > 240 {
			return fmt.Errorf("网站文件路径无效：%s", name)
		}
		for _, part := range strings.Split(name, "/") {
			if strings.HasPrefix(part, ".") && part != ".well-known" {
				return fmt.Errorf("网站不能包含隐藏文件：%s", name)
			}
		}
		size += len(data)
		if size > MaxWebsiteBytes {
			return fmt.Errorf("静态网站解压后不能超过 8 MiB")
		}
		for prefix := path.Dir(name); prefix != "."; prefix = path.Dir(prefix) {
			if _, ok := files[prefix]; ok {
				return fmt.Errorf("网站文件与目录冲突：%s", prefix)
			}
		}
	}
	return nil
}

// Literal loopback IPs avoid DNS resolving a purported local service elsewhere.
func ValidateWebsiteUpstream(raw string, publicPort int) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("本机服务应为 http://127.0.0.1:端口 或 http://[::1]:端口")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 || port == publicPort {
		return fmt.Errorf("本机服务必须使用回环 IP 和独立端口，不能转发回入口自身")
	}
	return nil
}

// WebsiteSpec joins the node's CDN front rules on the same Caddy listener.
// Files are a bounded public static bundle, never arbitrary host paths.
type WebsiteSpec struct {
	ID       string            `json:"id"`
	Domain   string            `json:"domain"`
	Port     int               `json:"port"`
	CertPEM  string            `json:"certPem"`
	KeyPEM   string            `json:"keyPem"`
	Path     string            `json:"path"`
	Mode     string            `json:"mode"`
	Upstream string            `json:"upstream,omitempty"`
	Files    map[string][]byte `json:"files,omitempty"`
	Proxies  []WebsiteProxy    `json:"proxies,omitempty"`
}

type WebsiteProxy struct {
	Path     string `json:"path"`
	Upstream string `json:"upstream"`
}

func (s *NodeSpec) HasWebFront() bool { return len(s.Fronts) > 0 || len(s.Websites) > 0 }
