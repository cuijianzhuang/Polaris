// Route MMDB databases (ASN and city/geo) share one policy, on-disk
// layout, HTTP handler set and upload flow; only the database type, size
// limit and user-facing strings differ. The shared implementation lives
// here, parameterised by routeDatabaseKind; route_asn.go and route_geo.go
// hold the per-kind configuration, thin handler wrappers and the
// enrichment code, whose per-hop mapping differs between the two kinds.
package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/cuijianzhuang/polaris/internal/panel/routedb"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// routeDatabaseEntry is one loaded MMDB snapshot, keyed by its SHA-256.
type routeDatabaseEntry[DB any] struct {
	hash string
	db   DB
}

// routeDatabaseKind describes one route MMDB kind (ASN or city/geo).
type routeDatabaseKind[DB any] struct {
	policyKey string
	dir       string // under cfg.DataDir, e.g. "route-asn"
	audit     string // audit event prefix, e.g. "route-asn"
	maxBytes  int64
	tooLarge  string
	// invalidPolicy is returned when the stored policy is malformed. It
	// is a sentinel for kinds whose GET handler reports an invalid policy
	// instead of a server error.
	invalidPolicy error
	conflict      error
	missing       error
	createDirErr  string
	tempErr       string
	writeErr      string
	installErr    string
	// decodeUpload normalises an upload (geo accepts gzip); nil = identity.
	decodeUpload func([]byte) ([]byte, error)
	fromBytes    func([]byte) (DB, error)
	metadata     func(DB) any
	lookup       func(DB, string) any
	// invalidPolicyJSON reports a malformed stored policy as a 200 with
	// databaseState "invalid_policy" instead of a 500.
	invalidPolicyJSON bool
}

// readRouteDatabasePolicy lives in routedb now (store.go); the SQL is
// dbStore in db_store.go.
func lockRouteDatabasePolicy(tx *gorm.DB, key string) (store.Setting, error) {
	row := store.Setting{Key: key, Value: `{"enabled":false,"sha256":"","revision":0}`}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return row, err
	}
	if err := tx.Model(&store.Setting{}).Where("key = ?", key).UpdateColumn("key", gorm.Expr("key")).Error; err != nil {
		return row, err
	}
	err := tx.First(&row, "key = ?", key).Error
	return row, err
}

func loadRouteDatabase[DB any](s *Server, kind routeDatabaseKind[DB], hash string, mu *sync.Mutex, cache **routeDatabaseEntry[DB]) (DB, string) {
	var zero DB
	if !routedb.Hash.MatchString(hash) {
		return zero, "not_configured"
	}
	mu.Lock()
	defer mu.Unlock()
	if *cache != nil && (*cache).hash == hash {
		return (*cache).db, "ready"
	}
	path := filepath.Join(s.cfg.DataDir, kind.dir, hash+".mmdb")
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return zero, "local_database_missing"
		}
		return zero, "local_database_unreadable"
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, kind.maxBytes+1))
	if err != nil || int64(len(raw)) > kind.maxBytes {
		return zero, "invalid_database"
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != hash {
		return zero, "hash_mismatch"
	}
	db, err := kind.fromBytes(raw)
	if err != nil {
		return zero, "invalid_database"
	}
	*cache = &routeDatabaseEntry[DB]{hash: hash, db: db}
	return db, "ready"
}

func getRouteDatabase[DB any](c *gin.Context, s *Server, kind routeDatabaseKind[DB], load func(string) (DB, string)) {
	p, err := routedb.ReadPolicy(dbStore{s.st.DB}, kind.policyKey, kind.invalidPolicy)
	if err != nil {
		if kind.invalidPolicyJSON && errors.Is(err, kind.invalidPolicy) {
			c.JSON(http.StatusOK, gin.H{"policy": routedb.Policy{}, "databaseState": "invalid_policy", "database": nil, "effectiveEnabled": false})
			return
		}
		serverError(c, err)
		return
	}
	db, state := load(p.SHA256)
	var metadata any
	if state == "ready" {
		metadata = kind.metadata(db)
	}
	c.JSON(http.StatusOK, gin.H{"policy": p, "databaseState": state, "database": metadata, "effectiveEnabled": p.Enabled && state == "ready"})
}

// Preview the installed snapshot even while annotations are disabled. This
// endpoint never resolves names, performs network requests, or changes policy.
func lookupRouteDatabase[DB any](c *gin.Context, s *Server, kind routeDatabaseKind[DB], load func(string) (DB, string)) {
	var req struct {
		IP string `json:"ip"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, errors.New("请输入有效的 IPv4 或 IPv6 地址"))
		return
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(req.IP))
	if err != nil || ip.Zone() != "" {
		badRequest(c, errors.New("请输入有效的 IPv4 或 IPv6 地址"))
		return
	}
	p, err := routedb.ReadPolicy(dbStore{s.st.DB}, kind.policyKey, kind.invalidPolicy)
	if err != nil {
		serverError(c, err)
		return
	}
	db, state := load(p.SHA256)
	if state != "ready" {
		c.JSON(http.StatusConflict, gin.H{"error": kind.missing.Error(), "databaseState": state})
		return
	}
	c.JSON(http.StatusOK, gin.H{"match": kind.lookup(db, ip.String()), "databaseSHA256": p.SHA256, "effectiveEnabled": p.Enabled})
}

func putRouteDatabase[DB any](c *gin.Context, s *Server, kind routeDatabaseKind[DB], load func(string) (DB, string), get func(*gin.Context)) {
	var req struct {
		Enabled          bool   `json:"enabled"`
		ExpectedRevision *int64 `json:"expectedRevision"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.ExpectedRevision != nil && *req.ExpectedRevision < 0 {
		badRequest(c, errors.New("expectedRevision 无效"))
		return
	}
	prior, readErr := routedb.ReadPolicy(dbStore{s.st.DB}, kind.policyKey, kind.invalidPolicy)
	if req.Enabled {
		if readErr != nil {
			serverError(c, readErr)
			return
		}
		if _, state := load(prior.SHA256); state != "ready" {
			c.JSON(http.StatusConflict, gin.H{"error": kind.missing.Error()})
			return
		}
	}
	changed := false
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		row, err := lockRouteDatabasePolicy(tx, kind.policyKey)
		if err != nil {
			return err
		}
		p, err := routedb.DecodePolicy(row.Value, kind.invalidPolicy)
		if err != nil {
			if req.Enabled {
				return err
			}
			p = routedb.Policy{}
		}
		if req.ExpectedRevision != nil && *req.ExpectedRevision != p.Revision {
			return kind.conflict
		}
		if req.Enabled && (p.Revision != prior.Revision || p.SHA256 != prior.SHA256) {
			return kind.conflict
		}
		if err == nil && p.Enabled == req.Enabled {
			return nil
		}
		p.Enabled = req.Enabled
		p.Revision++
		value, _ := json.Marshal(p)
		if err := tx.Model(&row).Update("value", string(value)).Error; err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, kind.conflict) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		serverError(c, err)
		return
	}
	if changed {
		s.auditCtx(c, kind.audit+".policy.update", "", "")
	}
	get(c)
}

// Binary uploads never accept an arbitrary server path or a remote URL.
func uploadRouteDatabase[DB any](c *gin.Context, s *Server, kind routeDatabaseKind[DB], load func(string) (DB, string), get func(*gin.Context), mu *sync.Mutex, cache **routeDatabaseEntry[DB]) {
	if c.Request.ContentLength > kind.maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": kind.tooLarge})
		return
	}
	var expected *int64
	if v, ok := c.GetQuery("expectedRevision"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			badRequest(c, errors.New("expectedRevision 无效"))
			return
		}
		expected = &n
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, kind.maxBytes+1))
	if err != nil {
		badRequest(c, err)
		return
	}
	if int64(len(raw)) > kind.maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": kind.tooLarge})
		return
	}
	if kind.decodeUpload != nil {
		raw, err = kind.decodeUpload(raw)
		if err != nil {
			badRequest(c, err)
			return
		}
	}
	db, err := kind.fromBytes(raw)
	if err != nil {
		badRequest(c, err)
		return
	}
	digest := sha256.Sum256(raw)
	hash := hex.EncodeToString(digest[:])
	dir := filepath.Join(s.cfg.DataDir, kind.dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		serverError(c, errors.New(kind.createDirErr))
		return
	}
	temp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		serverError(c, errors.New(kind.tempErr))
		return
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(raw); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		serverError(c, errors.New(kind.writeErr))
		return
	}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		row, err := lockRouteDatabasePolicy(tx, kind.policyKey)
		if err != nil {
			return err
		}
		p, decodeErr := routedb.DecodePolicy(row.Value, kind.invalidPolicy)
		if decodeErr != nil {
			p = routedb.Policy{}
		} // repair defaults off
		if expected != nil && *expected != p.Revision {
			return kind.conflict
		}
		oldHash := p.SHA256
		// The global setting lock also serializes placement/GC on shared disks.
		if err := os.Rename(temp.Name(), filepath.Join(dir, hash+".mmdb")); err != nil {
			return errors.New(kind.installErr)
		}
		if p.SHA256 != hash || decodeErr != nil {
			p.SHA256 = hash
			p.Revision++
			value, _ := json.Marshal(p)
			if err := tx.Model(&row).Update("value", string(value)).Error; err != nil {
				return err
			}
		}
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() || filepath.Ext(name) != ".mmdb" {
					continue
				}
				candidate := name[:len(name)-len(".mmdb")]
				if routedb.Hash.MatchString(candidate) && candidate != oldHash && candidate != hash {
					_ = os.Remove(filepath.Join(dir, name))
				}
			}
		}
		return nil
	})
	if errors.Is(err, kind.conflict) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		serverError(c, err)
		return
	}
	mu.Lock()
	*cache = &routeDatabaseEntry[DB]{hash: hash, db: db}
	mu.Unlock()
	s.auditCtx(c, kind.audit+".database.upload", hash, "")
	get(c)
}
