package panel

import (
	"errors"
	"time"

	"github.com/cuijianzhuang/polaris/internal/geoip"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/panel/routedb"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const routeGeoPolicyKey = "route_geo_policy"

var routeGeoHash = routedb.Hash
var errRouteGeoPolicy = errors.New("地理归属配置无效")
var errRouteGeoConflict = errors.New("地理归属配置已变化，请刷新后重试")
var errRouteGeoMissing = errors.New("请先向当前实例上传城市数据库")

// routeGeoKind is the city-database flavour of the shared route-database
// machinery in route_database.go; only the database type, size limit and
// user-facing strings differ from the ASN database.
var routeGeoKind = routeDatabaseKind[*geoip.Database]{
	policyKey:         routeGeoPolicyKey,
	dir:               "route-geo",
	audit:             "route-geo",
	maxBytes:          geoip.MaxDatabaseBytes,
	tooLarge:          "数据库不能超过 128 MiB",
	invalidPolicy:     errRouteGeoPolicy,
	invalidPolicyJSON: true,
	conflict:          errRouteGeoConflict,
	missing:           errRouteGeoMissing,
	createDirErr:      "无法创建城市数据库目录",
	tempErr:           "无法暂存城市数据库",
	writeErr:          "写入城市数据库失败",
	installErr:        "安装城市数据库失败",
	decodeUpload:      geoip.DecodeUpload,
	fromBytes:         geoip.FromBytes,
	metadata: func(db *geoip.Database) any {
		value := db.Metadata
		return &value
	},
	lookup: func(db *geoip.Database, ip string) any { return db.Lookup(ip) },
}

func decodeRouteGeoPolicy(value string) (routedb.Policy, error) {
	return routedb.DecodePolicy(value, errRouteGeoPolicy)
}

func readRouteGeoPolicy(db *gorm.DB) (routedb.Policy, error) {
	return readRouteDatabasePolicy(db, routeGeoPolicyKey, errRouteGeoPolicy)
}

func (s *Server) loadRouteGeo(hash string) (*geoip.Database, string) {
	return loadRouteDatabase(s, routeGeoKind, hash, &s.routeGeoMu, &s.routeGeoCache)
}

func (s *Server) getRouteGeo(c *gin.Context) {
	getRouteDatabase(c, s, routeGeoKind, s.loadRouteGeo)
}

func (s *Server) lookupRouteGeo(c *gin.Context) {
	lookupRouteDatabase(c, s, routeGeoKind, s.loadRouteGeo)
}

func (s *Server) putRouteGeo(c *gin.Context) {
	putRouteDatabase(c, s, routeGeoKind, s.loadRouteGeo, s.getRouteGeo)
}

func (s *Server) uploadRouteGeo(c *gin.Context) {
	uploadRouteDatabase(c, s, routeGeoKind, s.loadRouteGeo, s.getRouteGeo, &s.routeGeoMu, &s.routeGeoCache)
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
