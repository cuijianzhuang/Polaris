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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/geoip"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const routeGeoPolicyKey = "route_geo_policy"

var routeGeoHash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var errRouteGeoPolicy = errors.New("地理归属配置无效")
var errRouteGeoConflict = errors.New("地理归属配置已变化，请刷新后重试")
var errRouteGeoMissing = errors.New("请先向当前实例上传城市数据库")

type routeGeoPolicy struct {
	Enabled  bool   `json:"enabled"`
	SHA256   string `json:"sha256"`
	Revision int64  `json:"revision"`
}

type routeGeoDatabase struct {
	hash string
	db   *geoip.Database
}

func decodeRouteGeoPolicy(value string) (routeGeoPolicy, error) {
	var p routeGeoPolicy
	if value == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(value), &p); err != nil {
		return p, errRouteGeoPolicy
	}
	if p.Revision < 0 || (p.SHA256 != "" && !routeGeoHash.MatchString(p.SHA256)) || (p.Enabled && p.SHA256 == "") {
		return p, errRouteGeoPolicy
	}
	return p, nil
}

func readRouteGeoPolicy(db *gorm.DB) (routeGeoPolicy, error) {
	var row store.Setting
	err := db.First(&row, "key = ?", routeGeoPolicyKey).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return routeGeoPolicy{}, nil
	}
	if err != nil {
		return routeGeoPolicy{}, err
	}
	return decodeRouteGeoPolicy(row.Value)
}

func lockRouteGeoPolicy(tx *gorm.DB) (store.Setting, error) {
	row := store.Setting{Key: routeGeoPolicyKey, Value: `{"enabled":false,"sha256":"","revision":0}`}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return row, err
	}
	if err := tx.Model(&store.Setting{}).Where("key = ?", routeGeoPolicyKey).UpdateColumn("key", gorm.Expr("key")).Error; err != nil {
		return row, err
	}
	err := tx.First(&row, "key = ?", routeGeoPolicyKey).Error
	return row, err
}

func (s *Server) loadRouteGeo(hash string) (*geoip.Database, string) {
	if !routeGeoHash.MatchString(hash) {
		return nil, "not_configured"
	}
	s.routeGeoMu.Lock()
	defer s.routeGeoMu.Unlock()
	if s.routeGeoCache != nil && s.routeGeoCache.hash == hash {
		return s.routeGeoCache.db, "ready"
	}
	path := filepath.Join(s.cfg.DataDir, "route-geo", hash+".mmdb")
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "local_database_missing"
		}
		return nil, "local_database_unreadable"
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, geoip.MaxDatabaseBytes+1))
	if err != nil || len(raw) > geoip.MaxDatabaseBytes {
		return nil, "invalid_database"
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != hash {
		return nil, "hash_mismatch"
	}
	db, err := geoip.FromBytes(raw)
	if err != nil {
		return nil, "invalid_database"
	}
	s.routeGeoCache = &routeGeoDatabase{hash: hash, db: db}
	return db, "ready"
}

func (s *Server) getRouteGeo(c *gin.Context) {
	p, err := readRouteGeoPolicy(s.st.DB)
	if errors.Is(err, errRouteGeoPolicy) {
		c.JSON(http.StatusOK, gin.H{"policy": routeGeoPolicy{}, "databaseState": "invalid_policy", "database": nil, "effectiveEnabled": false})
		return
	}
	if err != nil {
		serverError(c, err)
		return
	}
	db, state := s.loadRouteGeo(p.SHA256)
	var metadata *geoip.Metadata
	if db != nil {
		value := db.Metadata
		metadata = &value
	}
	c.JSON(http.StatusOK, gin.H{"policy": p, "databaseState": state, "database": metadata, "effectiveEnabled": p.Enabled && db != nil})
}

// Preview the installed snapshot even while annotations are disabled. This
// endpoint never resolves names, performs network requests, or changes policy.
func (s *Server) lookupRouteGeo(c *gin.Context) {
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
	p, err := readRouteGeoPolicy(s.st.DB)
	if err != nil {
		serverError(c, err)
		return
	}
	db, state := s.loadRouteGeo(p.SHA256)
	if db == nil {
		c.JSON(http.StatusConflict, gin.H{"error": errRouteGeoMissing.Error(), "databaseState": state})
		return
	}
	c.JSON(http.StatusOK, gin.H{"match": db.Lookup(ip.String()), "databaseSHA256": p.SHA256, "effectiveEnabled": p.Enabled})
}

func (s *Server) putRouteGeo(c *gin.Context) {
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
	prior, readErr := readRouteGeoPolicy(s.st.DB)
	if req.Enabled {
		if readErr != nil {
			serverError(c, readErr)
			return
		}
		if db, _ := s.loadRouteGeo(prior.SHA256); db == nil {
			c.JSON(http.StatusConflict, gin.H{"error": errRouteGeoMissing.Error()})
			return
		}
	}
	changed := false
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		row, err := lockRouteGeoPolicy(tx)
		if err != nil {
			return err
		}
		p, err := decodeRouteGeoPolicy(row.Value)
		if err != nil {
			if req.Enabled {
				return err
			}
			p = routeGeoPolicy{}
		}
		if req.ExpectedRevision != nil && *req.ExpectedRevision != p.Revision {
			return errRouteGeoConflict
		}
		if req.Enabled && (p.Revision != prior.Revision || p.SHA256 != prior.SHA256) {
			return errRouteGeoConflict
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
	if errors.Is(err, errRouteGeoConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		serverError(c, err)
		return
	}
	if changed {
		s.auditCtx(c, "route-geo.policy.update", "", "")
	}
	s.getRouteGeo(c)
}

// Binary uploads never accept an arbitrary server path or a remote URL.
func (s *Server) uploadRouteGeo(c *gin.Context) {
	if c.Request.ContentLength > geoip.MaxDatabaseBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "数据库不能超过 128 MiB"})
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
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, geoip.MaxDatabaseBytes+1))
	if err != nil {
		badRequest(c, err)
		return
	}
	if len(raw) > geoip.MaxDatabaseBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "数据库不能超过 128 MiB"})
		return
	}
	raw, err = geoip.DecodeUpload(raw)
	if err != nil {
		badRequest(c, err)
		return
	}
	db, err := geoip.FromBytes(raw)
	if err != nil {
		badRequest(c, err)
		return
	}
	digest := sha256.Sum256(raw)
	hash := hex.EncodeToString(digest[:])
	dir := filepath.Join(s.cfg.DataDir, "route-geo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		serverError(c, errors.New("无法创建城市数据库目录"))
		return
	}
	temp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		serverError(c, errors.New("无法暂存城市数据库"))
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
		serverError(c, errors.New("写入城市数据库失败"))
		return
	}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		row, err := lockRouteGeoPolicy(tx)
		if err != nil {
			return err
		}
		p, decodeErr := decodeRouteGeoPolicy(row.Value)
		if decodeErr != nil {
			p = routeGeoPolicy{}
		} // repair defaults off
		if expected != nil && *expected != p.Revision {
			return errRouteGeoConflict
		}
		oldHash := p.SHA256
		// The global setting lock also serializes placement/GC on shared disks.
		if err := os.Rename(temp.Name(), filepath.Join(dir, hash+".mmdb")); err != nil {
			return errors.New("安装城市数据库失败")
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
				if routeGeoHash.MatchString(candidate) && candidate != oldHash && candidate != hash {
					_ = os.Remove(filepath.Join(dir, name))
				}
			}
		}
		return nil
	})
	if errors.Is(err, errRouteGeoConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		serverError(c, err)
		return
	}
	s.routeGeoMu.Lock()
	s.routeGeoCache = &routeGeoDatabase{hash: hash, db: db}
	s.routeGeoMu.Unlock()
	s.auditCtx(c, "route-geo.database.upload", hash, "")
	s.getRouteGeo(c)
}

// Always discard Agent-supplied attribution. This snapshot is panel-owned,
// remains in history, and is never recomputed while reading old records.
func (s *Server) enrichRouteGeo(results []model.RouteTraceResult) {
	p, err := readRouteGeoPolicy(s.st.DB)
	state := "disabled"
	reason := ""
	var db *geoip.Database
	if err != nil {
		state = "unavailable"
		reason = "policy_unavailable"
	} else if p.Enabled {
		db, reason = s.loadRouteGeo(p.SHA256)
		state = "unavailable"
		if db != nil {
			state = "ready"
			reason = ""
		}
	}
	for i := range results {
		r := &results[i]
		r.Geo = &model.RouteTraceGeoMetadata{State: state, Reason: reason, At: time.Now().UTC()}
		if err == nil && p.Enabled && routeGeoHash.MatchString(p.SHA256) {
			r.Geo.DatabaseSHA256 = p.SHA256
		}
		for j := range r.Hops {
			r.Hops[j].Geo = nil
		}
		if db == nil {
			continue
		}
		r.Geo.DatabaseType = db.Metadata.DatabaseType
		r.Geo.BuildEpoch = db.Metadata.BuildEpoch
		r.Geo.IPVersion = db.Metadata.IPVersion
		for j := range r.Hops {
			h := &r.Hops[j]
			if len(h.Addresses) > 4 {
				r.Geo.Limited = true
			}
			for _, ip := range h.Addresses[:min(len(h.Addresses), 4)] {
				match := db.Lookup(ip)
				h.Geo = append(h.Geo, model.RouteTraceIPGeo{State: match.State, IP: match.IP, CountryCode: match.CountryCode, Country: match.Country, CountryEN: match.CountryEN, Province: match.Province, ProvinceEN: match.ProvinceEN, City: match.City, CityEN: match.CityEN, Network: match.Network})
			}
		}
	}
}
