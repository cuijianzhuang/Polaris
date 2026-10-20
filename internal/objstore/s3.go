// Package objstore is a small client for S3-compatible object storage (AWS
// S3, Cloudflare R2, MinIO, Backblaze B2…): put, get, delete and list, signed
// with AWS Signature Version 4.
package objstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// S3 addresses one bucket. Endpoint is the service root ("" = AWS for the
// region); objects are addressed path-style (endpoint/bucket/key) unless
// VirtualHost is set (bucket.endpoint/key).
type S3 struct {
	Endpoint    string
	Region      string
	Bucket      string
	AccessKey   string
	SecretKey   string
	VirtualHost bool
	Client      *http.Client
	now         func() time.Time // tests
}

// Object is one listed key.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// ErrNotFound is returned by Get for a missing key.
var ErrNotFound = errors.New("object not found")

func (c *S3) region() string {
	if c.Region == "" {
		return "us-east-1"
	}
	return c.Region
}

func (c *S3) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

// base returns scheme, host and the path prefix of the bucket.
func (c *S3) base() (scheme, host, prefix string, err error) {
	ep := strings.TrimRight(c.Endpoint, "/")
	if ep == "" {
		ep = "https://s3." + c.region() + ".amazonaws.com"
	}
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", "", "", fmt.Errorf("S3 端点格式应为 https://主机")
	}
	if c.Bucket == "" {
		return "", "", "", fmt.Errorf("未填写存储桶")
	}
	if c.VirtualHost {
		return u.Scheme, c.Bucket + "." + u.Host, u.Path, nil
	}
	return u.Scheme, u.Host, u.Path + "/" + escape(c.Bucket), nil
}

// do signs and sends a request. body may be nil; for a file body the
// payload hash is computed by reading it once (the request rewinds it).
func (c *S3) do(ctx context.Context, method, key string, query url.Values, body io.ReadSeeker, size int64) (*http.Response, error) {
	scheme, host, prefix, err := c.base()
	if err != nil {
		return nil, err
	}
	uri := prefix + "/"
	if key != "" {
		uri = prefix + "/" + escape(key)
	}
	payload := emptyHash
	if body != nil {
		h := sha256.New()
		if _, err := io.Copy(h, body); err != nil {
			return nil, err
		}
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		payload = hex.EncodeToString(h.Sum(nil))
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	amzDate := now.UTC().Format("20060102T150405Z")
	q := canonicalQuery(query)
	headers := map[string]string{"host": host, "x-amz-content-sha256": payload, "x-amz-date": amzDate}
	auth := Sign(method, uri, q, headers, payload, c.region(), "s3", c.AccessKey, c.SecretKey, now)
	full := scheme + "://" + host + uri
	if q != "" {
		full += "?" + q
	}
	var rd io.Reader
	if body != nil {
		rd = body
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	req.Header.Set("x-amz-content-sha256", payload)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("Authorization", auth)
	return c.client().Do(req)
}

func check(resp *http.Response, what string) error {
	if resp.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
	msg := strings.TrimSpace(string(b))
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(b, &e) == nil && e.Code != "" {
		msg = e.Code + ": " + e.Message
	}
	return fmt.Errorf("S3 %s 失败：%s %s", what, resp.Status, msg)
}

// PutFile uploads a local file as key.
func (c *S3) PutFile(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return c.Put(ctx, key, f, fi.Size())
}

// Put uploads size bytes from body as key.
func (c *S3) Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	resp, err := c.do(ctx, http.MethodPut, key, nil, body, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return check(resp, "上传")
}

// Get opens key for reading; the caller closes it.
func (c *S3) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil, 0)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, ErrNotFound
	}
	if err := check(resp, "下载"); err != nil {
		resp.Body.Close()
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// Delete removes key (a missing key is not an error).
func (c *S3) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return check(resp, "删除")
}

// List returns every object whose key starts with prefix (ListObjectsV2,
// following continuation tokens).
func (c *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, 0)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = check(resp, "列出")
		if err == nil {
			err = xml.NewDecoder(resp.Body).Decode(&page)
		}
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			out = append(out, Object{Key: o.Key, Size: o.Size, Modified: o.LastModified})
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// --- Signature Version 4 ---

var emptyHash = hex.EncodeToString(sha256.New().Sum(nil))

// escape percent-encodes a key for the canonical URI: RFC 3986 unreserved
// characters stay, "/" separates segments.
func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || strings.IndexByte("-_.~/", ch) >= 0 {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func queryEscape(s string) string {
	return strings.ReplaceAll(escape(s), "/", "%2F")
}

// canonicalQuery sorts and encodes query parameters as SigV4 requires.
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, queryEscape(k)+"="+queryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// Sign returns the Authorization header for a request. uri is the
// canonical (escaped) path, query the canonical query string, headers the
// lower-case headers to sign.
func Sign(method, uri, query string, headers map[string]string, payloadHash, region, service, ak, sk string, now time.Time) string {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{method, uri, query, canonHeaders.String(), signed, payloadHash}, "\n")
	date := now.UTC().Format("20060102")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + now.UTC().Format("20060102T150405Z") + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+sk), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	return "AWS4-HMAC-SHA256 Credential=" + ak + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig
}
