package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/objstore"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// backupStore is where backup archives live: the local <data>/backups
// directory (default) or an S3-compatible bucket (backup_store=s3), which
// every panel instance sharing the database can list and restore from.
type backupStore interface {
	// where describes the location for the UI ("/var/lib/polaris/backups",
	// "s3://bucket/prefix").
	where() string
	// put moves the finished archive at path into the store as name. The
	// local store renames it; S3 uploads it and leaves path to the caller.
	put(ctx context.Context, name, path string) error
	list(ctx context.Context) ([]backupFile, error)
	open(ctx context.Context, name string) (io.ReadCloser, int64, error)
	remove(ctx context.Context, name string) error
}

var errBackupNotFound = errors.New("not found")

// backupStoreFor returns the configured store.
func backupStoreFor(st *store.Store, dataDir string) (backupStore, error) {
	if st.GetSetting("backup_store", "local") != "s3" {
		return &localStore{dir: filepath.Join(dataDir, backupDir)}, nil
	}
	c, prefix := s3FromSettings(st.GetSetting, st.Box.MustOpen(st.GetSetting("backup_s3_secret_key", "")))
	if c.Bucket == "" || c.AccessKey == "" || c.SecretKey == "" {
		return nil, errors.New("备份存储选了 S3，但存储桶或密钥没有填完整")
	}
	return &s3Store{c: c, prefix: prefix}, nil
}

// s3FromSettings builds the client from backup_s3_* values.
func s3FromSettings(get func(string, string) string, secretKey string) (*objstore.S3, string) {
	return &objstore.S3{
		Endpoint:    strings.TrimSpace(get("backup_s3_endpoint", "")),
		Region:      strings.TrimSpace(get("backup_s3_region", "")),
		Bucket:      strings.TrimSpace(get("backup_s3_bucket", "")),
		AccessKey:   strings.TrimSpace(get("backup_s3_access_key", "")),
		SecretKey:   secretKey,
		VirtualHost: get("backup_s3_path_style", "true") == "false",
	}, s3Prefix(get("backup_s3_prefix", ""))
}

// s3Prefix normalises a key prefix: no leading "/", a trailing "/" when set.
func s3Prefix(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p != "" {
		p += "/"
	}
	return p
}

// backupTime reads the creation time from the archive name, falling back to
// the given time.
func backupTime(name string, fallback time.Time) time.Time {
	if len(name) >= 23 {
		if t, err := time.ParseInLocation("20060102-150405", name[8:23], time.Local); err == nil {
			return t
		}
	}
	return fallback
}

func sortBackups(fs []backupFile) {
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].Created.Equal(fs[j].Created) {
			return fs[i].Name > fs[j].Name
		}
		return fs[i].Created.After(fs[j].Created)
	})
}

// --- local directory ---

type localStore struct{ dir string }

func (l *localStore) where() string { return l.dir }

func (l *localStore) put(_ context.Context, name, path string) error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(l.dir, name)
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s 已存在", name)
	}
	return os.Rename(path, dst)
}

func (l *localStore) list(context.Context) ([]backupFile, error) {
	ents, err := os.ReadDir(l.dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	out := []backupFile{}
	for _, e := range ents {
		if !backupNameRe.MatchString(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, backupFile{Name: e.Name(), Size: fi.Size(), Created: fi.ModTime(), Encrypted: strings.HasSuffix(e.Name(), ".enc")})
		}
	}
	sortBackups(out)
	return out, nil
}

func (l *localStore) open(_ context.Context, name string) (io.ReadCloser, int64, error) {
	f, err := os.Open(filepath.Join(l.dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, errBackupNotFound
		}
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

func (l *localStore) remove(_ context.Context, name string) error {
	err := os.Remove(filepath.Join(l.dir, name))
	if os.IsNotExist(err) {
		return errBackupNotFound
	}
	return err
}

// --- S3 ---

type s3Store struct {
	c      *objstore.S3
	prefix string
}

func (s *s3Store) where() string { return "s3://" + s.c.Bucket + "/" + s.prefix }

func (s *s3Store) put(ctx context.Context, name, path string) error {
	return s.c.PutFile(ctx, s.prefix+name, path)
}

func (s *s3Store) list(ctx context.Context) ([]backupFile, error) {
	objs, err := s.c.List(ctx, s.prefix)
	if err != nil {
		return nil, err
	}
	out := []backupFile{}
	for _, o := range objs {
		name := strings.TrimPrefix(o.Key, s.prefix)
		if !backupNameRe.MatchString(name) { // also skips keys in "sub/folders"
			continue
		}
		out = append(out, backupFile{Name: name, Size: o.Size, Created: backupTime(name, o.Modified), Encrypted: strings.HasSuffix(name, ".enc")})
	}
	sortBackups(out)
	return out, nil
}

func (s *s3Store) open(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	rc, n, err := s.c.Get(ctx, s.prefix+name)
	if errors.Is(err, objstore.ErrNotFound) {
		return nil, 0, errBackupNotFound
	}
	return rc, n, err
}

func (s *s3Store) remove(ctx context.Context, name string) error {
	return s.c.Delete(ctx, s.prefix+name)
}

// testS3 writes, reads, lists and deletes a small object to check the
// settings before they are used.
func testS3(ctx context.Context, c *objstore.S3, prefix string) error {
	key := prefix + fmt.Sprintf(".polaris-test-%d", time.Now().UnixNano())
	body := "polaris backup store test"
	if err := c.Put(ctx, key, strings.NewReader(body), int64(len(body))); err != nil {
		return err
	}
	defer c.Delete(context.WithoutCancel(ctx), key) //nolint:errcheck
	rc, _, err := c.Get(ctx, key)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(rc, 1024))
	rc.Close()
	if err != nil {
		return err
	}
	if string(b) != body {
		return errors.New("读回的内容和写入的不一致")
	}
	if _, err := c.List(ctx, prefix); err != nil {
		return err
	}
	return c.Delete(ctx, key)
}

// backupStoreTest: POST /backup/store/test {endpoint, region, bucket, prefix,
// accessKey, secretKey, pathStyle} checks S3 settings before saving them; an
// empty secretKey means the saved one.
func (s *Server) backupStoreTest(c *gin.Context) {
	var b struct {
		Endpoint  string `json:"endpoint"`
		Region    string `json:"region"`
		Bucket    string `json:"bucket"`
		Prefix    string `json:"prefix"`
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
		PathStyle *bool  `json:"pathStyle"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, err)
		return
	}
	vals := map[string]string{"backup_s3_endpoint": b.Endpoint, "backup_s3_region": b.Region, "backup_s3_bucket": b.Bucket,
		"backup_s3_prefix": b.Prefix, "backup_s3_access_key": b.AccessKey, "backup_s3_path_style": "true"}
	if b.PathStyle != nil && !*b.PathStyle {
		vals["backup_s3_path_style"] = "false"
	}
	secretKey := b.SecretKey
	if secretKey == "" {
		secretKey = s.st.Box.MustOpen(s.st.GetSetting("backup_s3_secret_key", ""))
	}
	cl, prefix := s3FromSettings(func(k, def string) string {
		if v := vals[k]; v != "" {
			return v
		}
		return def
	}, secretKey)
	if cl.Bucket == "" || cl.AccessKey == "" || cl.SecretKey == "" {
		badRequest(c, errors.New("请填写存储桶、Access Key 和 Secret Key"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	if err := testS3(ctx, cl, prefix); err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
