package panel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cuijianzhuang/polaris/internal/asn"
	"github.com/cuijianzhuang/polaris/internal/geoip"
	"github.com/gin-gonic/gin"
)

const routeDatabaseReleases = "https://api.github.com/repos/lwpk110/free-geoip-databases/releases?per_page=20"

var errRepositoryDownload = errors.New("仓库下载失败，请稍后重试或切换来源")
var errRepositorySize = errors.New("仓库数据库超过大小限制")
var errRepositoryHash = errors.New("仓库文件 SHA-256 校验失败，请重试或切换来源")
var repositoryASNAsset = regexp.MustCompile(`^GeoLite2-ASN-([0-9]{8})\.mmdb$`)
var repositoryCityAsset = regexp.MustCompile(`^GeoLite2-City-([0-9]{8})\.mmdb$`)

// Requests originate only from fixed repository presets. Guard every redirect
// as well as each socket, regardless of the subscription private-source opt-in.
func validDatabaseRepositoryURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	switch u.Hostname() {
	case "raw.githubusercontent.com":
		return strings.HasPrefix(u.Path, "/Loyalsoldier/geoip/") || strings.HasPrefix(u.Path, "/wp-statistics/GeoLite2-City/")
	case "api.github.com":
		return u.Path == "/repos/lwpk110/free-geoip-databases/releases"
	case "github.com":
		return strings.HasPrefix(u.Path, "/lwpk110/free-geoip-databases/releases/download/")
	case "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	case "cdn.jsdelivr.net":
		return strings.HasPrefix(u.Path, "/gh/Loyalsoldier/geoip@release/") || u.Path == "/npm/geolite2-city/GeoLite2-City.mmdb.gz"
	}
	return false
}

type repositoryTransport struct{ inner http.RoundTripper }

func (r repositoryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !validDatabaseRepositoryURL(req.URL) {
		return nil, errRepositoryDownload
	}
	return r.inner.RoundTrip(req)
}
func databaseProxyAddress(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func newRepositoryDatabaseClient(transport http.RoundTripper) *http.Client {
	if transport == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, addr string, _ syscall.RawConn) error {
			host, _, e := net.SplitHostPort(addr)
			if e != nil {
				return e
			}
			return checkSourceIP(net.ParseIP(host), false)
		}}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		// Environment proxies are operator-configured connection endpoints. Permit
		// their sockets while still checking the public destination before proxying.
		proxies := map[string]bool{}
		for _, host := range []string{"raw.githubusercontent.com", "api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com", "cdn.jsdelivr.net"} {
			proxy, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: host}})
			if err == nil && proxy != nil {
				proxies[databaseProxyAddress(proxy)] = true
			}
		}
		proxyDialer := &net.Dialer{Timeout: 10 * time.Second}
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if proxies[address] {
				return proxyDialer.DialContext(ctx, network, address)
			}
			return dialer.DialContext(ctx, network, address)
		}
		tr.Proxy = sourceProxy(false, http.ProxyFromEnvironment, net.DefaultResolver.LookupIP)
		transport = tr
	}
	return &http.Client{Timeout: 90 * time.Second, Transport: repositoryTransport{inner: transport}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !validDatabaseRepositoryURL(req.URL) {
			return errRepositoryDownload
		}
		return nil
	}}
}
func repositoryBytes(ctx context.Context, client *http.Client, rawURL string, limit int64) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if e != nil {
		return nil, errRepositoryDownload
	}
	req.Header.Set("User-Agent", "Polaris-Database-Importer")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Accept", "application/octet-stream")
	if req.URL.Hostname() == "api.github.com" {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	res, e := client.Do(req)
	if e != nil {
		return nil, errRepositoryDownload
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errRepositoryDownload
	}
	if res.ContentLength > limit {
		return nil, errRepositorySize
	}
	raw, e := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if e != nil {
		return nil, errRepositoryDownload
	}
	if int64(len(raw)) > limit {
		return nil, errRepositorySize
	}
	return raw, nil
}
func repositoryPreset(kind, source string) (file, checksum string, ok bool) {
	if kind == "asn" {
		switch source {
		case "loyalsoldier":
			file = "https://raw.githubusercontent.com/Loyalsoldier/geoip/release/GeoLite2-ASN.mmdb"
		case "loyalsoldier-cdn":
			file = "https://cdn.jsdelivr.net/gh/Loyalsoldier/geoip@release/GeoLite2-ASN.mmdb"
		case "free-geoip":
			return "", "", true
		default:
			return "", "", false
		}
		return file, file + ".sha256sum", true
	}
	if kind == "geo" {
		switch source {
		case "wp-statistics":
			return "https://raw.githubusercontent.com/wp-statistics/GeoLite2-City/master/GeoLite2-City.mmdb.gz", "", true
		case "wp-statistics-cdn":
			return "https://cdn.jsdelivr.net/npm/geolite2-city/GeoLite2-City.mmdb.gz", "", true
		case "free-geoip":
			return "", "", true
		}
	}
	return "", "", false
}
func downloadRepositoryDatabase(ctx context.Context, client *http.Client, kind, source string) ([]byte, error) {
	file, checksum, ok := repositoryPreset(kind, source)
	if !ok {
		return nil, errors.New("不支持的数据库仓库来源")
	}
	limit := int64(asn.MaxDatabaseBytes)
	if kind == "geo" {
		limit = geoip.MaxDatabaseBytes
	}
	if source == "wp-statistics" || source == "wp-statistics-cdn" {
		limit = geoip.MaxCompressedBytes
	}
	var expected string
	if source == "free-geoip" {
		raw, e := repositoryBytes(ctx, client, routeDatabaseReleases, 2<<20)
		if e != nil {
			return nil, e
		}
		var releases []struct {
			Tag        string `json:"tag_name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
			Assets     []struct {
				Name   string `json:"name"`
				Size   int64  `json:"size"`
				URL    string `json:"browser_download_url"`
				Digest string `json:"digest"`
			} `json:"assets"`
		}
		if json.Unmarshal(raw, &releases) != nil {
			return nil, errRepositoryDownload
		}
		pattern := repositoryASNAsset
		if kind == "geo" {
			pattern = repositoryCityAsset
		}
		for _, r := range releases {
			if r.Draft || r.Prerelease {
				continue
			}
			for _, a := range r.Assets {
				match := pattern.FindStringSubmatch(a.Name)
				if len(match) != 2 || r.Tag != "geolite2-"+match[1] {
					continue
				}
				u, e := url.Parse(a.URL)
				if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/lwpk110/free-geoip-databases/releases/download/"+r.Tag+"/"+a.Name || path.Base(u.Path) != a.Name {
					continue
				}
				if a.Size > limit {
					return nil, errRepositorySize
				}
				if a.Digest != "" {
					expected = strings.TrimPrefix(a.Digest, "sha256:")
					if !strings.HasPrefix(a.Digest, "sha256:") || !routeASNHash.MatchString(expected) {
						return nil, errRepositoryHash
					}
				}
				file = a.URL
				break
			}
			if file != "" {
				break
			}
		}
		if file == "" {
			return nil, errors.New("仓库未找到匹配的 ASN 或 City 数据库")
		}
	}
	if checksum != "" {
		raw, e := repositoryBytes(ctx, client, checksum, 4096)
		if e != nil {
			return nil, e
		}
		fields := strings.Fields(string(raw))
		if len(fields) < 1 || !routeASNHash.MatchString(fields[0]) {
			return nil, errRepositoryHash
		}
		expected = fields[0]
	}
	raw, e := repositoryBytes(ctx, client, file, limit)
	if e != nil {
		return nil, e
	}
	if expected != "" {
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != expected {
			return nil, errRepositoryHash
		}
	}
	return raw, nil
}
func (s *Server) fetchRouteDatabase(c *gin.Context, kind string) {
	var req struct {
		Source           string `json:"source"`
		ExpectedRevision *int64 `json:"expectedRevision"`
	}
	if e := c.ShouldBindJSON(&req); e != nil || req.ExpectedRevision == nil || *req.ExpectedRevision < 0 {
		badRequest(c, errors.New("请提供仓库来源和当前配置版本"))
		return
	}
	if _, _, ok := repositoryPreset(kind, req.Source); !ok {
		badRequest(c, errors.New("不支持的数据库仓库来源"))
		return
	}
	var revision int64
	if kind == "asn" {
		p, e := readRouteASNPolicy(s.st.DB)
		if e != nil {
			serverError(c, e)
			return
		}
		revision = p.Revision
	} else {
		p, e := readRouteGeoPolicy(s.st.DB)
		if e != nil && !errors.Is(e, errRouteGeoPolicy) {
			serverError(c, e)
			return
		}
		revision = p.Revision
	}
	if revision != *req.ExpectedRevision {
		c.JSON(http.StatusConflict, gin.H{"error": "数据库配置已变化，请刷新后重试"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	client := newRepositoryDatabaseClient(s.routeDatabaseTransport)
	defer client.CloseIdleConnections()
	raw, e := downloadRepositoryDatabase(ctx, client, kind, req.Source)
	if e != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": e.Error()})
		return
	}
	if ctx.Err() != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": errRepositoryDownload.Error()})
		return
	}
	// Reuse the same bounded parser, durable install and CAS transaction as upload.
	original := c.Request
	r := original.Clone(ctx)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	r.Header.Set("Content-Type", "application/octet-stream")
	q := r.URL.Query()
	q.Set("expectedRevision", strconv.FormatInt(*req.ExpectedRevision, 10))
	r.URL.RawQuery = q.Encode()
	c.Request = r
	defer func() { c.Request = original }()
	if kind == "asn" {
		s.uploadRouteASN(c)
	} else {
		s.uploadRouteGeo(c)
	}
}
func (s *Server) fetchRouteASN(c *gin.Context) { s.fetchRouteDatabase(c, "asn") }
func (s *Server) fetchRouteGeo(c *gin.Context) { s.fetchRouteDatabase(c, "geo") }
