package panel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/store"
)

const idemContextKey = "idempotencyRequest"
const idemBodyLimit = 8 << 20

var errIdempotencyConflict = errors.New("同一 Idempotency-Key 已用于不同请求，请核对订单")
var errLegacyIdempotency = errors.New("存在无法安全归属的旧版幂等记录，请先核对订单状态")

type idempotencyRequest struct{ Key, RequestHash, LegacyKey string }

type idemWriter struct {
	gin.ResponseWriter
	buf bytes.Buffer
}

func (w *idemWriter) Write(b []byte) (int, error) { w.buf.Write(b); return w.ResponseWriter.Write(b) }
func (w *idemWriter) WriteString(v string) (int, error) {
	w.buf.WriteString(v)
	return w.ResponseWriter.WriteString(v)
}

// Reference-count the locks: deleting a mutex while another request waits
// on it would allow a third request to run against a different mutex.
var idemLocks = struct {
	sync.Mutex
	entries map[string]*idemLock
}{entries: map[string]*idemLock{}}

type idemLock struct {
	sync.Mutex
	refs int
}

func lockIdempotency(key string) func() {
	idemLocks.Lock()
	l := idemLocks.entries[key]
	if l == nil {
		l = &idemLock{}
		idemLocks.entries[key] = l
	}
	l.refs++
	idemLocks.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		idemLocks.Lock()
		l.refs--
		if l.refs == 0 {
			delete(idemLocks.entries, key)
		}
		idemLocks.Unlock()
	}
}

func idemDigest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }

func idempotencyIdentity(c *gin.Context) string {
	if t := apiTokenOf(c); t != nil {
		return "token:" + t.ID
	}
	if id := c.GetString("adminID"); id != "" {
		return "admin:" + id
	}
	if v, ok := c.Get("tgAdmin"); ok {
		return "telegram:" + v.(*store.TGAdmin).ID
	}
	return "" // never fall back to a mutable display name
}

func (s *Server) newIdempotencyRequest(c *gin.Context, key string) (*idempotencyRequest, error) {
	body := c.Request.Body
	if body == nil {
		body = http.NoBody
	}
	raw, err := io.ReadAll(io.LimitReader(body, idemBodyLimit+1))
	_ = body.Close()
	if err != nil {
		return nil, err
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if len(raw) > idemBodyLimit {
		return nil, errors.New("带幂等键的请求正文不能超过 8 MiB")
	}
	normalized := raw
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value, extra any
	if d.Decode(&value) == nil && d.Decode(&extra) == io.EOF {
		normalized, _ = json.Marshal(value)
	}
	identity := idempotencyIdentity(c)
	if identity == "" {
		return nil, errors.New("无法确定幂等请求身份")
	}
	resource := strings.TrimPrefix(c.Request.URL.Path, s.cfg.APIBase())
	legacy := c.GetString("admin") + " " + c.Request.Method + " " + c.Request.URL.Path + " " + key
	if len(legacy) > 200 {
		legacy = idemDigest(legacy)
	}
	return &idempotencyRequest{
		Key:         idemDigest("v2\x00" + identity + "\x00" + c.Request.Method + "\x00" + resource + "\x00" + key),
		RequestHash: idemDigest(c.Request.URL.Query().Encode() + "\x00" + string(normalized)), LegacyKey: legacy,
	}, nil
}

func (s *Server) idempotency(c *gin.Context) {
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" || c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
		c.Next()
		return
	}
	if len(key) > 100 {
		badRequest(c, errors.New("Idempotency-Key 最长 100 个字符"))
		return
	}
	info, err := s.newIdempotencyRequest(c, key)
	if err != nil {
		badRequest(c, err)
		return
	}
	c.Set(idemContextKey, info)
	route := c.Request.Method + " " + strings.TrimPrefix(c.FullPath(), s.cfg.APIBase())
	if route == "POST /users/:id/renew" || route == "POST /users/:id/add-traffic" || route == "POST /users/:id/plan" {
		// These handlers reserve the key, mutate the user and store their answer
		// in one database transaction. A response-only cache cannot do that.
		c.Next()
		return
	}
	unlock := lockIdempotency(info.Key)
	defer unlock()
	var prev store.IdempotencyKey
	err = s.st.DB.Where("key = ? AND created_at > ?", info.Key, time.Now().Add(-24*time.Hour)).First(&prev).Error
	if err == nil {
		if prev.RequestHash != info.RequestHash {
			idempotencyConflict(c, errIdempotencyConflict)
			return
		}
		replayIdempotency(c, prev)
		return
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		serverError(c, err)
		return
	}
	if err = checkLegacyIdempotency(s.st.DB, info); err != nil {
		idempotencyError(c, err)
		return
	}
	w := &idemWriter{ResponseWriter: c.Writer}
	c.Writer = w
	c.Next()
	if status := w.Status(); status < 500 {
		result := store.IdempotencyKey{Key: info.Key, RequestHash: info.RequestHash, Status: status, Body: w.buf.Bytes(), CreatedAt: time.Now()}
		if err := s.st.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&result).Error; err != nil {
			s.log.Error("idempotency response cache", "err", err)
		}
	}
}

func checkLegacyIdempotency(tx *gorm.DB, info *idempotencyRequest) error {
	var n int64
	if err := tx.Model(&store.IdempotencyKey{}).Where("key = ? AND created_at > ?", info.LegacyKey, time.Now().Add(-24*time.Hour)).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return errLegacyIdempotency
	}
	return nil
}

// The first write acquires SQLite's writer reservation. PostgreSQL's
// unique index arbitrates same-key requests, waiting for the winner's commit.
func reserveUserCommand(tx *gorm.DB, info *idempotencyRequest) (*store.IdempotencyKey, error) {
	if info == nil {
		return nil, nil
	}
	if err := tx.Where("key = ? AND created_at <= ?", info.Key, time.Now().Add(-24*time.Hour)).Delete(&store.IdempotencyKey{}).Error; err != nil {
		return nil, err
	}
	row := store.IdempotencyKey{Key: info.Key, RequestHash: info.RequestHash, CreatedAt: time.Now()}
	inserted := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if inserted.Error != nil {
		return nil, inserted.Error
	}
	if inserted.RowsAffected == 0 {
		if err := tx.First(&row, "key = ?", info.Key).Error; err != nil {
			return nil, err
		}
		if row.RequestHash != info.RequestHash {
			return nil, errIdempotencyConflict
		}
		if row.Status == 0 {
			return nil, errors.New("幂等请求尚未完成，请稍后重试")
		}
		return &row, nil
	}
	if err := checkLegacyIdempotency(tx, info); err != nil {
		return nil, err
	}
	return nil, nil
}
func replayIdempotency(c *gin.Context, row store.IdempotencyKey) {
	c.Header("Idempotent-Replayed", "true")
	c.Data(row.Status, "application/json; charset=utf-8", row.Body)
	c.Abort()
}
func idempotencyConflict(c *gin.Context, err error) {
	c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
}
func idempotencyError(c *gin.Context, err error) {
	if errors.Is(err, errIdempotencyConflict) || errors.Is(err, errLegacyIdempotency) {
		idempotencyConflict(c, err)
		return
	}
	serverError(c, err)
}
func (s *Server) pruneIdempotency() {
	s.st.DB.Where("created_at < ?", time.Now().Add(-24*time.Hour)).Delete(&store.IdempotencyKey{})
}
