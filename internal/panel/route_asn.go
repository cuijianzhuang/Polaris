package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/cuijianzhuang/polaris/internal/asn"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const routeASNPolicyKey = "route_asn_policy"

var routeASNHash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var errRouteASNConflict = errors.New("ASN 配置已变化，请刷新后重试")
var errRouteASNMissing = errors.New("请先向当前实例上传 ASN 数据库")

type routeASNPolicy struct {
	Enabled  bool   `json:"enabled"`
	SHA256   string `json:"sha256"`
	Revision int64  `json:"revision"`
}

type routeASNDatabase struct {
	hash string
	db   *asn.Database
}

func decodeRouteASNPolicy(value string) (routeASNPolicy, error) {
	var p routeASNPolicy
	if value == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(value), &p); err != nil {
		return p, errors.New("ASN 配置无效")
	}
	if p.Revision < 0 || (p.SHA256 != "" && !routeASNHash.MatchString(p.SHA256)) || (p.Enabled && p.SHA256 == "") {
		return p, errors.New("ASN 配置无效")
	}
	return p, nil
}

func readRouteASNPolicy(db *gorm.DB) (routeASNPolicy, error) {
	var row store.Setting
	err := db.First(&row, "key = ?", routeASNPolicyKey).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return routeASNPolicy{}, nil
	}
	if err != nil {
		return routeASNPolicy{}, err
	}
	return decodeRouteASNPolicy(row.Value)
}

func lockRouteASNPolicy(tx *gorm.DB) (store.Setting, error) {
	row := store.Setting{Key: routeASNPolicyKey, Value: `{"enabled":false,"sha256":"","revision":0}`}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return row, err
	}
	if err := tx.Model(&store.Setting{}).Where("key = ?", routeASNPolicyKey).UpdateColumn("key", gorm.Expr("key")).Error; err != nil {
		return row, err
	}
	err := tx.First(&row, "key = ?", routeASNPolicyKey).Error
	return row, err
}

func (s *Server) loadRouteASN(hash string) (*asn.Database, string) {
	if !routeASNHash.MatchString(hash) {
		return nil, "not_configured"
	}
	s.routeASNMu.Lock()
	defer s.routeASNMu.Unlock()
	if s.routeASNCache != nil && s.routeASNCache.hash == hash {
		return s.routeASNCache.db, "ready"
	}
	path := filepath.Join(s.cfg.DataDir, "route-asn", hash+".mmdb")
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "local_database_missing"
		}
		return nil, "local_database_unreadable"
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, asn.MaxDatabaseBytes+1))
	if err != nil || len(raw) > asn.MaxDatabaseBytes {
		return nil, "invalid_database"
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != hash {
		return nil, "hash_mismatch"
	}
	db, err := asn.FromBytes(raw)
	if err != nil {
		return nil, "invalid_database"
	}
	s.routeASNCache = &routeASNDatabase{hash: hash, db: db}
	return db, "ready"
}

func (s *Server) getRouteASN(c *gin.Context) {
	p, err := readRouteASNPolicy(s.st.DB)
	if err != nil {
		serverError(c, err)
		return
	}
	db, state := s.loadRouteASN(p.SHA256)
	var metadata *asn.Metadata
	if db != nil {
		value := db.Metadata
		metadata = &value
	}
	c.JSON(http.StatusOK, gin.H{"policy": p, "databaseState": state, "database": metadata, "effectiveEnabled": p.Enabled && db != nil})
}

func (s *Server) putRouteASN(c *gin.Context) {
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
	prior, readErr := readRouteASNPolicy(s.st.DB)
	if req.Enabled {
		if readErr != nil {
			serverError(c, readErr)
			return
		}
		if db, _ := s.loadRouteASN(prior.SHA256); db == nil {
			c.JSON(http.StatusConflict, gin.H{"error": errRouteASNMissing.Error()})
			return
		}
	}
	changed := false
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		row, err := lockRouteASNPolicy(tx)
		if err != nil {
			return err
		}
		p, err := decodeRouteASNPolicy(row.Value)
		if err != nil {
			if req.Enabled {
				return err
			}
			p = routeASNPolicy{}
		}
		if req.ExpectedRevision != nil && *req.ExpectedRevision != p.Revision {
			return errRouteASNConflict
		}
		if req.Enabled && (p.Revision != prior.Revision || p.SHA256 != prior.SHA256) {
			return errRouteASNConflict
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
	if errors.Is(err, errRouteASNConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		serverError(c, err)
		return
	}
	if changed {
		s.auditCtx(c, "route-asn.policy.update", "", "")
	}
	s.getRouteASN(c)
}

// Binary uploads never accept an arbitrary server path or a remote URL.
func (s *Server) uploadRouteASN(c *gin.Context) {
	if c.Request.ContentLength > asn.MaxDatabaseBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "数据库不能超过 64 MiB"})
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
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, asn.MaxDatabaseBytes+1))
	if err != nil {
		badRequest(c, err)
		return
	}
	if len(raw) > asn.MaxDatabaseBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "数据库不能超过 64 MiB"})
		return
	}
	db, err := asn.FromBytes(raw)
	if err != nil {
		badRequest(c, err)
		return
	}
	digest := sha256.Sum256(raw)
	hash := hex.EncodeToString(digest[:])
	dir := filepath.Join(s.cfg.DataDir, "route-asn")
	if err := os.MkdirAll(dir, 0700); err != nil {
		serverError(c, errors.New("无法创建 ASN 数据目录"))
		return
	}
	temp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		serverError(c, errors.New("无法暂存 ASN 数据库"))
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
		serverError(c, errors.New("写入 ASN 数据库失败"))
		return
	}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		row, err := lockRouteASNPolicy(tx)
		if err != nil {
			return err
		}
		p, decodeErr := decodeRouteASNPolicy(row.Value)
		if decodeErr != nil {
			p = routeASNPolicy{}
		} // repair defaults off
		if expected != nil && *expected != p.Revision {
			return errRouteASNConflict
		}
		oldHash := p.SHA256
		// The global setting lock also serializes placement/GC on shared disks.
		if err := os.Rename(temp.Name(), filepath.Join(dir, hash+".mmdb")); err != nil {
			return errors.New("安装 ASN 数据库失败")
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
				if routeASNHash.MatchString(candidate) && candidate != oldHash && candidate != hash {
					_ = os.Remove(filepath.Join(dir, name))
				}
			}
		}
		return nil
	})
	if errors.Is(err, errRouteASNConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		serverError(c, err)
		return
	}
	s.routeASNMu.Lock()
	s.routeASNCache = &routeASNDatabase{hash: hash, db: db}
	s.routeASNMu.Unlock()
	s.auditCtx(c, "route-asn.database.upload", hash, "")
	s.getRouteASN(c)
}

// Always discard Agent-supplied attribution. This snapshot is panel-owned,
// remains in history, and is never recomputed while reading old records.
func (s *Server) enrichRouteASN(results []model.RouteTraceResult) {
	p, err := readRouteASNPolicy(s.st.DB)
	state := "disabled"
	reason := ""
	var db *asn.Database
	if err != nil {
		state = "unavailable"
		reason = "policy_unavailable"
	} else if p.Enabled {
		db, reason = s.loadRouteASN(p.SHA256)
		state = "unavailable"
		if db != nil {
			state = "ready"
			reason = ""
		}
	}
	for i := range results {
		r := &results[i]
		r.ASN = &model.RouteTraceASNMetadata{State: state, Reason: reason, At: time.Now().UTC()}
		if err == nil && p.Enabled && routeASNHash.MatchString(p.SHA256) {
			r.ASN.DatabaseSHA256 = p.SHA256
		}
		for j := range r.Hops {
			r.Hops[j].ASN = nil
		}
		if db == nil {
			continue
		}
		r.ASN.DatabaseType = db.Metadata.DatabaseType
		r.ASN.BuildEpoch = db.Metadata.BuildEpoch
		r.ASN.IPVersion = db.Metadata.IPVersion
		for j := range r.Hops {
			h := &r.Hops[j]
			if len(h.Addresses) > 4 {
				r.ASN.Limited = true
			}
			for _, ip := range h.Addresses[:min(len(h.Addresses), 4)] {
				match := db.Lookup(ip)
				h.ASN = append(h.ASN, model.RouteTraceIPASN{State: match.State, IP: match.IP, ASN: match.ASN, Organization: match.Organization, Network: match.Network})
			}
		}
	}
}
