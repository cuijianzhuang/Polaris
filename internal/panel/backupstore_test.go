package panel

import (
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// memS3 is a minimal path-style S3 for backup store tests (signatures are
// covered by the objstore tests).
type memS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (m *memS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") {
		w.WriteHeader(403)
		io.WriteString(w, "<Error><Code>InvalidAccessKeyId</Code><Message>no</Message></Error>")
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/bk/")
	if !ok && r.URL.Path != "/bk" {
		w.WriteHeader(404)
		return
	}
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case r.Method == "PUT":
		m.objs[key] = body
	case r.Method == "GET" && key == "":
		type obj struct {
			Key          string
			Size         int
			LastModified string
		}
		var res struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []obj
		}
		var keys []string
		for k := range m.objs {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			res.Contents = append(res.Contents, obj{k, len(m.objs[k]), time.Now().UTC().Format(time.RFC3339)})
		}
		xml.NewEncoder(w).Encode(res)
	case r.Method == "GET":
		b, ok := m.objs[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(b)
	case r.Method == "DELETE":
		delete(m.objs, key)
		w.WriteHeader(204)
	}
}

func (m *memS3) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func useS3(s *Server, endpoint string) {
	for k, v := range map[string]string{"backup_store": "s3", "backup_s3_endpoint": endpoint, "backup_s3_bucket": "bk",
		"backup_s3_prefix": "/polaris", "backup_s3_access_key": "AK"} {
		s.st.SetSetting(k, v)
	}
	s.st.SetSetting("backup_s3_secret_key", s.st.Box.Seal("SK"))
}

// Two panels sharing one bucket see the same backups.
func TestBackupS3Store(t *testing.T) {
	fake := &memS3{objs: map[string][]byte{}}
	s3srv := httptest.NewServer(fake)
	defer s3srv.Close()

	a, _, adminA := newAdminClient(t)
	b, _, adminB := newAdminClient(t)
	useS3(a, s3srv.URL)
	useS3(b, s3srv.URL)
	if got := a.pushTargets(); len(got) != 0 {
		t.Fatalf("the backup bucket is also a push target: %v", got)
	}

	st, out := adminA.do("POST", "/backup", nil)
	if st != 200 {
		t.Fatalf("create %d %v", st, out)
	}
	name := out["name"].(string)
	if k := fake.keys(); len(k) != 1 || k[0] != "polaris/"+name {
		t.Fatalf("bucket %v", k)
	}
	_, info := adminB.do("GET", "/backup", nil)
	files := info["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["name"] != name || info["dir"] != "s3://bk/polaris/" || info["store"] != "s3" {
		t.Fatalf("info on B %v", info)
	}
	req, _ := http.NewRequest("GET", adminB.base+"/backup/files/"+name, nil)
	req.Header.Set("Authorization", "Bearer "+adminB.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(archive) < 100 || archive[0] != 0x1f || !strings.Contains(resp.Header.Get("Content-Disposition"), name) {
		t.Fatalf("download %d %d bytes", resp.StatusCode, len(archive))
	}

	// Retention runs against the bucket.
	a.st.SetSetting("backup_keep", "2")
	for _, kind := range []string{"x", "y"} {
		if _, _, done, err := a.createBackup(kind); err != nil {
			t.Fatal(err)
		} else {
			done()
		}
		time.Sleep(1100 * time.Millisecond) // names have second resolution
	}
	if k := fake.keys(); len(k) != 2 {
		t.Fatalf("retention: %v", k)
	}
	_, info = adminB.do("GET", "/backup", nil)
	latest := info["files"].([]any)[0].(map[string]any)["name"].(string)
	if !strings.HasSuffix(latest, "-y.tar.gz") {
		t.Fatalf("newest first: %v", info["files"])
	}

	// B restores A's backup from the list; its next start keeps a snapshot
	// of the old data in the bucket.
	restarted := make(chan struct{}, 1)
	b.restartHook = func() { restarted <- struct{}{} }
	if st, out := adminB.do("POST", "/backup/files/"+latest+"/restore", map[string]any{}); st != 200 {
		t.Fatalf("restore %d %v", st, out)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("no restart")
	}
	b.st.Close()
	b2, err := New(b.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.st.Close()
	pre := 0
	for _, k := range fake.keys() {
		if strings.Contains(k, "-prerestore") {
			pre++
		}
	}
	if pre != 1 {
		t.Fatalf("snapshot not in bucket: %v", fake.keys())
	}

	// Delete through B, and a missing name is 204 on S3 (idempotent).
	if st, _ := adminA.do("DELETE", "/backup/files/"+latest, nil); st != 204 {
		t.Fatalf("delete %d", st)
	}
	if st, _ := adminA.do("GET", "/backup/files/"+latest, nil); st != 404 {
		t.Fatalf("deleted still served: %d", st)
	}

	// Connection test: saved secret, then a wrong access key.
	if st, out := adminA.do("POST", "/backup/store/test", map[string]any{"endpoint": s3srv.URL, "bucket": "bk", "accessKey": "AK"}); st != 200 || out["ok"] != true {
		t.Fatalf("test %d %v", st, out)
	}
	if st, out := adminA.do("POST", "/backup/store/test", map[string]any{"endpoint": s3srv.URL, "bucket": "bk", "accessKey": "BAD", "secretKey": "x"}); st != 200 || out["ok"] != false || !strings.Contains(out["error"].(string), "InvalidAccessKeyId") {
		t.Fatalf("bad key %d %v", st, out)
	}
	for _, k := range fake.keys() {
		if strings.Contains(k, "polaris-test") {
			t.Fatalf("test object left: %v", k)
		}
	}

	// An unreachable store is reported, not fatal.
	a.st.SetSetting("backup_s3_endpoint", "http://127.0.0.1:1")
	_, info = adminA.do("GET", "/backup", nil)
	if info["storeError"] == "" || len(info["files"].([]any)) != 0 {
		t.Fatalf("unreachable store: %v", info)
	}
}
