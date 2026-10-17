package panel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/cuijianzhuang/polaris/internal/license"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Off-site copies of backups: Telegram (owners' chats), WebDAV (PUT into a
// folder) and S3-compatible storage (AWS, Cloudflare R2, MinIO, Backblaze B2…
// signed with SigV4, path-style). Each is used when configured; failures are
// reported, never fatal.

// pushTargets lists the configured destinations.
func (s *Server) pushTargets() []string {
	var out []string
	if s.st.GetSetting("backup_tg", "false") == "true" {
		out = append(out, "telegram")
	}
	if s.st.GetSetting("backup_webdav_url", "") != "" {
		out = append(out, "webdav")
	}
	if s.st.GetSetting("backup_s3_bucket", "") != "" && s.st.GetSetting("backup_s3_access_key", "") != "" {
		out = append(out, "s3")
	}
	return out
}

// pushBackup copies an archive to every destination and returns target ->
// "ok" or the error. Telegram also receives manual pushes when it is the only
// way out (auto=false and the bot runs).
func (s *Server) pushBackup(path string, auto bool) map[string]string {
	res := map[string]string{}
	name := filepath.Base(path)
	data, err := os.ReadFile(path)
	if err != nil {
		res["read"] = err.Error()
		return res
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if !s.license().Allow(license.FeatureBackupPush) {
		res["license"] = "备份推送需要专业版许可证；本地备份已保存"
		return res
	}
	targets := s.pushTargets()
	if !auto && s.tg != nil && s.tg.isRunning() && !contains(targets, "telegram") {
		targets = append(targets, "telegram")
	}
	for _, t := range targets {
		var err error
		switch t {
		case "telegram":
			s.tg.sendBackup(path)
		case "webdav":
			err = webdavPut(ctx, s.st.GetSetting("backup_webdav_url", ""), s.st.GetSetting("backup_webdav_user", ""),
				s.st.Box.MustOpen(s.st.GetSetting("backup_webdav_pass", "")), name, data)
		case "s3":
			err = s3Put(ctx, s3Config{
				Endpoint: s.st.GetSetting("backup_s3_endpoint", "https://s3.amazonaws.com"), Region: s.st.GetSetting("backup_s3_region", "us-east-1"),
				Bucket: s.st.GetSetting("backup_s3_bucket", ""), AccessKey: s.st.GetSetting("backup_s3_access_key", ""),
				SecretKey: s.st.Box.MustOpen(s.st.GetSetting("backup_s3_secret_key", "")),
			}, strings.TrimLeft(s.st.GetSetting("backup_s3_prefix", ""), "/")+name, data, time.Now())
		}
		if err != nil {
			res[t] = err.Error()
			s.log.Error("backup push", "target", t, "err", err)
			s.notify(Event{Kind: "backup_failed", Text: fmt.Sprintf("⚠️ 备份 %s 推送到 %s 失败：%s", name, t, err)})
		} else {
			res[t] = "ok"
		}
	}
	return res
}

func checkUpload(resp *http.Response, err error, what string) error {
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s 返回 %s %s", what, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// webdavPut uploads data as <folder URL>/<name>.
func webdavPut(ctx context.Context, folder, user, pass, name string, data []byte) error {
	if !strings.HasPrefix(folder, "https://") && !strings.HasPrefix(folder, "http://") {
		return fmt.Errorf("WebDAV 地址应以 http(s):// 开头")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(folder, "/")+"/"+url.PathEscape(name), bytes.NewReader(data))
	if err != nil {
		return err
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	return checkUpload(resp, err, "WebDAV")
}

type s3Config struct {
	Endpoint, Region, Bucket, AccessKey, SecretKey string
}

// s3Put uploads data to bucket/key (path-style) with a SigV4 signature.
func s3Put(ctx context.Context, c s3Config, key string, data []byte, now time.Time) error {
	ep, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
	if err != nil || ep.Host == "" || (ep.Scheme != "https" && ep.Scheme != "http") {
		return fmt.Errorf("S3 端点格式应为 https://主机")
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	uri := ep.Path + "/" + s3Escape(c.Bucket) + "/" + s3Escape(key)
	sum := sha256.Sum256(data)
	payload := hex.EncodeToString(sum[:])
	amzDate := now.UTC().Format("20060102T150405Z")
	headers := map[string]string{"host": ep.Host, "x-amz-content-sha256": payload, "x-amz-date": amzDate}
	auth := sigV4(http.MethodPut, uri, "", headers, payload, c.Region, "s3", c.AccessKey, c.SecretKey, now)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, ep.Scheme+"://"+ep.Host+uri, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("x-amz-content-sha256", payload)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	return checkUpload(resp, err, "S3")
}

// s3Escape percent-encodes a key for the canonical URI (RFC 3986 unreserved
// characters stay, "/" separates segments).
func s3Escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || c == '/' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// sigV4 returns the Authorization header for a request whose signed headers
// (lower-case names) are given; uri is already encoded.
func sigV4(method, uri, query string, headers map[string]string, payloadHash, region, service, ak, sk string, now time.Time) string {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, k := range names {
		canon.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	creq := strings.Join([]string{method, uri, query, canon.String(), signed, payloadHash}, "\n")
	date := now.UTC().Format("20060102")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	h := sha256.Sum256([]byte(creq))
	sts := "AWS4-HMAC-SHA256\n" + now.UTC().Format("20060102T150405Z") + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	k := hmacSHA256([]byte("AWS4"+sk), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", ak, scope, signed, hex.EncodeToString(hmacSHA256(k, sts)))
}
