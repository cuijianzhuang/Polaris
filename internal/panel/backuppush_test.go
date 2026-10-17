package panel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSigV4 checks the signer against AWS's documented GET Object example.
func TestSigV4(t *testing.T) {
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	empty := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	auth := sigV4("GET", "/test.txt", "", map[string]string{
		"host": "examplebucket.s3.amazonaws.com", "range": "bytes=0-9", "x-amz-content-sha256": empty, "x-amz-date": "20130524T000000Z",
	}, empty, "us-east-1", "s3", "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", now)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if auth != want {
		t.Fatalf("got  %s\nwant %s", auth, want)
	}
}

func TestBackupPush(t *testing.T) {
	var got = map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut {
			w.WriteHeader(405)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/dav/") {
			if u, p, ok := r.BasicAuth(); !ok || u != "me" || p != "pw" {
				w.WriteHeader(401)
				return
			}
			got["webdav"] = r.URL.Path + ":" + string(b)
		} else {
			a := r.Header.Get("Authorization")
			if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AK/") || r.Header.Get("x-amz-content-sha256") == "" {
				w.WriteHeader(403)
				return
			}
			got["s3"] = r.URL.Path + ":" + string(b)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := webdavPut(ctx, srv.URL+"/dav/", "me", "pw", "b 1.tar.gz", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := webdavPut(ctx, srv.URL+"/dav", "me", "bad", "b.tar.gz", []byte("x")); err == nil {
		t.Fatal("bad credentials accepted")
	}
	if err := s3Put(ctx, s3Config{Endpoint: srv.URL, Bucket: "bk", AccessKey: "AK", SecretKey: "SK"}, "polaris/b.tar.gz", []byte("y"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got["webdav"] != "/dav/b 1.tar.gz:x" || got["s3"] != "/bk/polaris/b.tar.gz:y" {
		t.Fatalf("uploads: %v", got)
	}

	// Through the server: configured targets get the archive.
	s, _, admin := newAdminClient(t)
	licenseAll(s)
	s.st.SetSetting("backup_webdav_url", srv.URL+"/dav")
	s.st.SetSetting("backup_webdav_user", "me")
	s.st.SetSetting("backup_webdav_pass", s.st.Box.Seal("pw"))
	s.st.SetSetting("backup_s3_endpoint", srv.URL)
	s.st.SetSetting("backup_s3_bucket", "bk")
	s.st.SetSetting("backup_s3_access_key", "AK")
	s.st.SetSetting("backup_s3_secret_key", s.st.Box.Seal("SK"))
	s.st.SetSetting("backup_s3_prefix", "polaris/")
	code, out := admin.do("POST", "/backup?push=1", nil)
	push, _ := out["push"].(map[string]any)
	if code != 200 || push["webdav"] != "ok" || push["s3"] != "ok" {
		t.Fatalf("push %d %v", code, out)
	}
	name := out["name"].(string)
	if !strings.HasPrefix(got["s3"], "/bk/polaris/"+name+":") {
		t.Fatalf("s3 object: %.60s", got["s3"])
	}
}
