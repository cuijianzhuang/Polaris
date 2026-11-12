package panel

import (
	"errors"
	"time"

	"github.com/cuijianzhuang/polaris/internal/asn"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/panel/routedb"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const routeASNPolicyKey = "route_asn_policy"

var routeASNHash = routedb.Hash
var errRouteASNInvalid = errors.New("ASN 配置无效")
var errRouteASNConflict = errors.New("ASN 配置已变化，请刷新后重试")
var errRouteASNMissing = errors.New("请先向当前实例上传 ASN 数据库")

// routeASNKind is the ASN flavour of the shared route-database machinery
// in route_database.go; only the database type, size limit and user-facing
// strings differ from the city database.
var routeASNKind = routeDatabaseKind[*asn.Database]{
	policyKey:     routeASNPolicyKey,
	dir:           "route-asn",
	audit:         "route-asn",
	maxBytes:      asn.MaxDatabaseBytes,
	tooLarge:      "数据库不能超过 64 MiB",
	invalidPolicy: errRouteASNInvalid,
	conflict:      errRouteASNConflict,
	missing:       errRouteASNMissing,
	createDirErr:  "无法创建 ASN 数据目录",
	tempErr:       "无法暂存 ASN 数据库",
	writeErr:      "写入 ASN 数据库失败",
	installErr:    "安装 ASN 数据库失败",
	fromBytes:     asn.FromBytes,
	metadata: func(db *asn.Database) any {
		value := db.Metadata
		return &value
	},
	lookup: func(db *asn.Database, ip string) any { return db.Lookup(ip) },
}

func decodeRouteASNPolicy(value string) (routedb.Policy, error) {
	return routedb.DecodePolicy(value, errRouteASNInvalid)
}

func readRouteASNPolicy(db *gorm.DB) (routedb.Policy, error) {
	return readRouteDatabasePolicy(db, routeASNPolicyKey, errRouteASNInvalid)
}

func (s *Server) loadRouteASN(hash string) (*asn.Database, string) {
	return loadRouteDatabase(s, routeASNKind, hash, &s.routeASNMu, &s.routeASNCache)
}

func (s *Server) getRouteASN(c *gin.Context) {
	getRouteDatabase(c, s, routeASNKind, s.loadRouteASN)
}

func (s *Server) lookupRouteASN(c *gin.Context) {
	lookupRouteDatabase(c, s, routeASNKind, s.loadRouteASN)
}

func (s *Server) putRouteASN(c *gin.Context) {
	putRouteDatabase(c, s, routeASNKind, s.loadRouteASN, s.getRouteASN)
}

func (s *Server) uploadRouteASN(c *gin.Context) {
	uploadRouteDatabase(c, s, routeASNKind, s.loadRouteASN, s.getRouteASN, &s.routeASNMu, &s.routeASNCache)
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
