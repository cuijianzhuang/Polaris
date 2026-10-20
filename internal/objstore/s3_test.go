package objstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Vectors from the AWS documentation (SigV4 test suite "get-vanilla" and the
// S3 "GET object" example).
func TestSignVectors(t *testing.T) {
	cases := []struct {
		name, method, uri, query string
		headers                  map[string]string
		payload, region, service string
		ak, sk                   string
		at                       string
		want                     string
	}{
		{
			name: "get-vanilla", method: "GET", uri: "/",
			headers: map[string]string{"host": "example.amazonaws.com", "x-amz-date": "20150830T123600Z"},
			payload: emptyHash, region: "us-east-1", service: "service",
			ak: "AKIDEXAMPLE", sk: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", at: "20150830T123600Z",
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			name: "s3-get-object", method: "GET", uri: "/test.txt",
			headers: map[string]string{"host": "examplebucket.s3.amazonaws.com", "range": "bytes=0-9", "x-amz-content-sha256": emptyHash, "x-amz-date": "20130524T000000Z"},
			payload: emptyHash, region: "us-east-1", service: "s3",
			ak: "AKIAIOSFODNN7EXAMPLE", sk: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", at: "20130524T000000Z",
			want: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name: "s3-list", method: "GET", uri: "/", query: canonicalQuery(url.Values{"max-keys": {"2"}, "prefix": {"J"}}),
			headers: map[string]string{"host": "examplebucket.s3.amazonaws.com", "x-amz-content-sha256": emptyHash, "x-amz-date": "20130524T000000Z"},
			payload: emptyHash, region: "us-east-1", service: "s3",
			ak: "AKIAIOSFODNN7EXAMPLE", sk: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", at: "20130524T000000Z",
			want: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for _, c := range cases {
		at, _ := time.Parse("20060102T150405Z", c.at)
		got := Sign(c.method, c.uri, c.query, c.headers, c.payload, c.region, c.service, c.ak, c.sk, at)
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

func TestEscape(t *testing.T) {
	if got := escape("a b/ü+~"); got != "a%20b/%C3%BC%2B~" {
		t.Fatal(got)
	}
	if got := canonicalQuery(url.Values{"prefix": {"x/y z"}, "a": {"1"}}); got != "a=1&prefix=x%2Fy%20z" {
		t.Fatal(got)
	}
}

// fakeS3 is an in-memory S3 that checks every signature and pages listings
// two keys at a time.
type fakeS3 struct {
	t       *testing.T
	ak, sk  string
	bucket  string
	vhost   bool
	mu      sync.Mutex
	objects map[string][]byte
	lists   int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Verify the signature exactly as the client computed it.
	payload := r.Header.Get("x-amz-content-sha256")
	body, _ := io.ReadAll(r.Body)
	if r.Method == http.MethodPut {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != payload {
			http.Error(w, "<Error><Code>XAmzContentSHA256Mismatch</Code></Error>", 400)
			return
		}
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("x-amz-date"))
	if err != nil {
		http.Error(w, "bad date", 400)
		return
	}
	headers := map[string]string{"host": r.Host, "x-amz-content-sha256": payload, "x-amz-date": r.Header.Get("x-amz-date")}
	want := Sign(r.Method, r.URL.EscapedPath(), canonicalQuery(r.URL.Query()), headers, payload, "us-east-1", "s3", f.ak, f.sk, at)
	if r.Header.Get("Authorization") != want {
		w.WriteHeader(403)
		io.WriteString(w, "<Error><Code>SignatureDoesNotMatch</Code><Message>bad signature</Message></Error>")
		return
	}
	path := r.URL.Path
	if f.vhost {
		if !strings.HasPrefix(r.Host, f.bucket+".") {
			http.Error(w, "wrong host", 400)
			return
		}
	} else {
		if !strings.HasPrefix(path, "/"+f.bucket+"/") && path != "/"+f.bucket {
			http.Error(w, "<Error><Code>NoSuchBucket</Code></Error>", 404)
			return
		}
		path = strings.TrimPrefix(path, "/"+f.bucket)
	}
	key := strings.TrimPrefix(path, "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPut:
		f.objects[key] = body
	case r.Method == http.MethodGet && key == "":
		f.list(w, r.URL.Query())
	case r.Method == http.MethodGet:
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, q url.Values) {
	f.lists++
	if q.Get("list-type") != "2" {
		http.Error(w, "v1", 400)
		return
	}
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, q.Get("prefix")) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := 0
	if tok := q.Get("continuation-token"); tok != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(tok, "tok-"))
	}
	type content struct {
		Key          string
		Size         int
		LastModified string
	}
	var res struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		Contents              []content
		IsTruncated           bool
		NextContinuationToken string `xml:",omitempty"`
	}
	end := min(start+2, len(keys))
	for _, k := range keys[start:end] {
		res.Contents = append(res.Contents, content{Key: k, Size: len(f.objects[k]), LastModified: "2026-10-20T01:02:03.000Z"})
	}
	if end < len(keys) {
		res.IsTruncated = true
		res.NextContinuationToken = "tok-" + strconv.Itoa(end)
	}
	xml.NewEncoder(w).Encode(res)
}

func newFake(t *testing.T, vhost bool) (*fakeS3, *S3) {
	f := &fakeS3{t: t, ak: "AK", sk: "SK", bucket: "bk", vhost: vhost, objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	c := &S3{Endpoint: srv.URL, Bucket: "bk", AccessKey: "AK", SecretKey: "SK", VirtualHost: vhost}
	if vhost {
		// bk.127.0.0.1 does not resolve: dial the test server whatever the host.
		_, port, _ := net.SplitHostPort(addr)
		c.Endpoint = "http://s3.test:" + port
		c.Client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}}
	}
	return f, c
}

func TestS3RoundTrip(t *testing.T) {
	for _, vhost := range []bool{false, true} {
		t.Run(map[bool]string{false: "path", true: "vhost"}[vhost], func(t *testing.T) {
			f, c := newFake(t, vhost)
			ctx := context.Background()
			for _, k := range []string{"p/a.tar.gz", "p/b c.tar.gz", "p/c.tar.gz", "p/d+e.tar.gz", "p/e.tar.gz", "other/x"} {
				data := []byte("data-" + k)
				if err := c.Put(ctx, k, bytes.NewReader(data), int64(len(data))); err != nil {
					t.Fatalf("put %s: %v", k, err)
				}
			}
			objs, err := c.List(ctx, "p/")
			if err != nil {
				t.Fatal(err)
			}
			if len(objs) != 5 || f.lists != 3 {
				t.Fatalf("list: %d objects in %d pages", len(objs), f.lists)
			}
			if objs[1].Key != "p/b c.tar.gz" || objs[1].Size != int64(len("data-p/b c.tar.gz")) || objs[1].Modified.IsZero() {
				t.Fatalf("object: %+v", objs[1])
			}
			rc, n, err := c.Get(ctx, "p/d+e.tar.gz")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			if string(b) != "data-p/d+e.tar.gz" || n != int64(len(b)) {
				t.Fatalf("get: %q %d", b, n)
			}
			if _, _, err := c.Get(ctx, "p/missing"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing: %v", err)
			}
			if err := c.Delete(ctx, "p/a.tar.gz"); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(ctx, "p/a.tar.gz"); err != nil {
				t.Fatal(err)
			}
			objs, _ = c.List(ctx, "p/")
			if len(objs) != 4 {
				t.Fatalf("after delete: %d", len(objs))
			}
		})
	}
}

func TestS3Errors(t *testing.T) {
	_, c := newFake(t, false)
	c.SecretKey = "wrong"
	err := c.Put(context.Background(), "k", bytes.NewReader(nil), 0)
	if err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := (&S3{Endpoint: "ftp://x", Bucket: "b"}).List(context.Background(), ""); err == nil {
		t.Fatal("bad endpoint accepted")
	}
	if _, err := (&S3{Region: "eu-west-1"}).List(context.Background(), ""); err == nil {
		t.Fatal("missing bucket accepted")
	}
}
