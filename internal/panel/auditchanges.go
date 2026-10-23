package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Change records (audit_changes): while an admin request runs, every row it
// creates, modifies or deletes in the configuration tables is written to the
// audit log as "change.create|update|delete" with the changed fields (old →
// new) and the account. Credentials are only marked as changed, and fields
// the system rewrites on its own (traffic counters, heartbeats, probe state)
// are left out. Writes outside admin requests (schedules, agents) are not
// recorded. The request is identified by its goroutine: handlers write to
// the database synchronously.

// changeTables lists the recorded tables (by model) and the Go fields of
// each that are bookkeeping, not configuration.
var changeTables = map[string][]string{
	"nodes":             {"AgentVersion", "AgentFeatures", "Arch", "Status", "LastSeen", "DesiredRev", "AppliedRev", "LastError", "System", "TokenHash", "TokenExpires", "CertSerial", "PublicIPv4", "PublicIPv6", "IPChangedAt", "NetRx", "NetTx", "NetCycleStart", "NetWarned", "NetLastRx", "NetLastTx", "NetBaseline", "NetEpoch", "NetBootID", "NetSampleUptime", "NetObservedAt", "NetBaselineAt", "NetBaselineReason", "NetLastGapAt", "NetLastGapSeconds"},
	"users":             {"UsedUp", "UsedDown", "LastSubAt", "LastSubUA", "LastResetAt", "OnlineAt", "BannedUntil", "OnlineIPs", "WarnedUsage", "WarnedExpire", "RenewCount"},
	"inbounds":          {},
	"cores":             {"Status", "PID", "StartedAt", "Error", "InstalledVersions", "RunningVersion", "PrevVersion", "NoUserTraffic"},
	"groups":            {},
	"group_inbounds":    {},
	"group_nodes":       {},
	"user_groups":       {},
	"user_accesses":     {},
	"outbounds":         {},
	"chains":            {"Probe", "ProbedAt"},
	"routing_rules":     {},
	"inbound_templates": {},
	"endpoints":         {},
	"cdn_profiles":      {},
	"certificates":      {"LastError"},
	"settings":          {},
	"sub_templates":     {},
	"sub_sources":       {"Cache", "NodeCount", "UserInfo", "FetchedAt", "LastError"},
	"sub_profiles":      {},
	"dns_providers":     {},
	"domain_bindings":   {"Current", "Status", "LastError", "SyncedAt"},
	"tg_admins":         {},
	"forward_rules":     {"Health"},
	"user_node_quotas":  {"Used", "ExtraUsed"},
	"admins":            {"FailedAttempts", "LockedUntil", "LastLoginAt", "TokenVersion"},
}

// Settings the panel writes for itself.
var volatileSettings = []string{"_last", "seen_base_url", "update_notified", "access_last", "traffic_daily_backfilled", "auto_update_failed", "update_then_agents"}

// sensitiveFields are recorded as "changed", never with their values.
var sensitiveFields = map[string]bool{
	"PasswordHash": true, "TOTPSecret": true, "TOTPPending": true, "RecoveryCodes": true, "SubToken": true, "UUID": true,
	"Password": true, "SSSeed": true, "Key": true, "Token": true, "TunnelAuth": true, "Credentials": true,
}

type changeActor struct{ name, ip string }

type changeLog struct {
	on     sync.Mutex
	actors map[uint64]changeActor
}

func goid() uint64 {
	var buf [64]byte
	b := buf[:runtime.Stack(buf[:], false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	if i := bytes.IndexByte(b, ' '); i > 0 {
		n, _ := strconv.ParseUint(string(b[:i]), 10, 64)
		return n
	}
	return 0
}

func (s *Server) changeLogOn() bool { return s.auditChanges.Load() }

// trackChanges marks the request's goroutine with its admin while a
// mutating request runs (only when change records are on).
func (s *Server) trackChanges(c *gin.Context) {
	if c.Request.Method == "GET" || !s.changeLogOn() {
		c.Next()
		return
	}
	id := goid()
	s.changes.on.Lock()
	if s.changes.actors == nil {
		s.changes.actors = map[uint64]changeActor{}
	}
	s.changes.actors[id] = changeActor{name: c.GetString("admin"), ip: clientIP(c.Request)}
	s.changes.on.Unlock()
	defer func() {
		s.changes.on.Lock()
		delete(s.changes.actors, id)
		s.changes.on.Unlock()
	}()
	c.Next()
}

func (s *Server) actorFor() (changeActor, bool) {
	if !s.changeLogOn() {
		return changeActor{}, false
	}
	s.changes.on.Lock()
	defer s.changes.on.Unlock()
	if len(s.changes.actors) == 0 {
		return changeActor{}, false
	}
	a, ok := s.changes.actors[goid()]
	return a, ok
}

// registerChangeLog installs the GORM callbacks.
func (s *Server) registerChangeLog() {
	cb := s.st.DB.Callback()
	_ = cb.Create().Before("gorm:create").Register("polaris:changes_before_create", s.changeBefore)
	_ = cb.Create().After("gorm:create").Register("polaris:changes_after_create", func(db *gorm.DB) { s.changeAfter(db, "create") })
	_ = cb.Update().Before("gorm:update").Register("polaris:changes_before_update", s.changeBefore)
	_ = cb.Update().After("gorm:update").Register("polaris:changes_after_update", func(db *gorm.DB) { s.changeAfter(db, "update") })
	_ = cb.Delete().Before("gorm:delete").Register("polaris:changes_before_delete", s.changeBefore)
	_ = cb.Delete().After("gorm:delete").Register("polaris:changes_after_delete", func(db *gorm.DB) { s.changeAfter(db, "delete") })
}

type rowSnap map[string]any // db column -> value

func recorded(db *gorm.DB) (*schema.Schema, map[string]bool, bool) {
	st := db.Statement
	if st.Schema == nil || db.Error != nil {
		return nil, nil, false
	}
	ignore, ok := changeTables[st.Schema.Table]
	if !ok {
		return nil, nil, false
	}
	skip := map[string]bool{"UpdatedAt": true, "CreatedAt": true}
	for _, f := range ignore {
		skip[f] = true
	}
	return st.Schema, skip, true
}

// pkConds returns WHERE conditions for the rows in the statement's model
// value (struct or slice) that carry primary keys.
func pkConds(db *gorm.DB, sch *schema.Schema) []map[string]any {
	rv := db.Statement.ReflectValue
	var out []map[string]any
	add := func(v reflect.Value) {
		for v.Kind() == reflect.Ptr {
			if v.IsNil() {
				return
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct || len(sch.PrimaryFields) == 0 {
			return
		}
		cond := map[string]any{}
		for _, f := range sch.PrimaryFields {
			val, zero := f.ValueOf(context.Background(), v)
			if zero {
				return
			}
			cond[f.DBName] = val
		}
		out = append(out, cond)
	}
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			add(rv.Index(i))
		}
	default:
		add(rv)
	}
	return out
}

// snapshot reads the rows a statement is about to touch (in its own
// transaction / connection).
func snapshot(db *gorm.DB, sch *schema.Schema) []rowSnap {
	q := db.Session(&gorm.Session{NewDB: true, SkipHooks: true}).Table(sch.Table)
	var rows []map[string]any
	if conds := pkConds(db, sch); len(conds) > 0 {
		for _, c := range conds {
			var r []map[string]any
			q.Session(&gorm.Session{}).Where(c).Limit(1).Find(&r)
			rows = append(rows, r...)
		}
	} else if w, ok := db.Statement.Clauses["WHERE"]; ok {
		q.Clauses(w.Expression).Limit(200).Find(&rows)
	}
	out := make([]rowSnap, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

func (s *Server) changeBefore(db *gorm.DB) {
	if _, ok := s.actorFor(); !ok {
		return
	}
	sch, _, ok := recorded(db)
	if !ok {
		return
	}
	db.InstanceSet("polaris:before", snapshot(db, sch))
}

func pkOf(sch *schema.Schema, r rowSnap) string {
	var parts []string
	for _, f := range sch.PrimaryFields {
		parts = append(parts, fmt.Sprint(r[f.DBName]))
	}
	return strings.Join(parts, "/")
}

// labelOf names a row for the log: table/pk (name).
func labelOf(sch *schema.Schema, r rowSnap) string {
	l := sch.Table + "/" + pkOf(sch, r)
	for _, c := range []string{"username", "name", "tag", "domain", "key"} {
		if v, ok := r[c]; ok && v != nil && fmt.Sprint(v) != "" && fmt.Sprint(v) != pkOf(sch, r) {
			return l + "（" + fmt.Sprint(v) + "）"
		}
	}
	return l
}

func shown(f *schema.Field, v any) any {
	if v == nil {
		return nil
	}
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	if str, ok := v.(string); ok && (sensitiveFields[f.Name] || strings.Contains(str, "enc:v1:")) {
		return "（已隐藏）"
	}
	if t, ok := v.(time.Time); ok {
		return t.Local().Format("2006-01-02 15:04:05")
	}
	return v
}

func same(a, b any) bool {
	if ab, ok := a.([]byte); ok {
		a = string(ab)
	}
	if bb, ok := b.([]byte); ok {
		b = string(bb)
	}
	if at, ok := a.(time.Time); ok {
		if bt, ok := b.(time.Time); ok {
			return at.Equal(bt)
		}
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func (s *Server) changeAfter(db *gorm.DB, op string) {
	actor, ok := s.actorFor()
	if !ok || db.Error != nil {
		return
	}
	sch, skip, ok := recorded(db)
	if !ok {
		return
	}
	var before []rowSnap
	if v, ok := db.InstanceGet("polaris:before"); ok {
		before, _ = v.([]rowSnap)
	}
	after := map[string]rowSnap{}
	if op != "delete" {
		for _, r := range snapshot(db, sch) {
			after[pkOf(sch, r)] = r
		}
		for _, r := range before { // where-updates: re-read the same rows
			if _, ok := after[pkOf(sch, r)]; !ok {
				cond := map[string]any{}
				for _, f := range sch.PrimaryFields {
					cond[f.DBName] = r[f.DBName]
				}
				var rr []map[string]any
				db.Session(&gorm.Session{NewDB: true, SkipHooks: true}).Table(sch.Table).Where(cond).Limit(1).Find(&rr)
				for _, x := range rr {
					after[pkOf(sch, x)] = x
				}
			}
		}
	}
	old := map[string]rowSnap{}
	for _, r := range before {
		old[pkOf(sch, r)] = r
	}
	write := func(kind string, row rowSnap, diff map[string][2]any) {
		if sch.Table == "settings" {
			k := fmt.Sprint(row["key"])
			for _, v := range volatileSettings {
				if strings.Contains(k, v) {
					return
				}
			}
		}
		detail, _ := json.Marshal(diff)
		db.Session(&gorm.Session{NewDB: true, SkipHooks: true}).Create(&store.AuditLog{Admin: actor.name, Action: "change." + kind,
			Target: truncate(labelOf(sch, row), 250), Detail: truncate(string(detail), 4000), IP: actor.ip})
	}
	fieldDiff := func(o, n rowSnap) map[string][2]any {
		d := map[string][2]any{}
		for _, f := range sch.Fields {
			if f.DBName == "" || skip[f.Name] {
				continue
			}
			ov, nv := o[f.DBName], n[f.DBName]
			if o != nil && n != nil && same(ov, nv) {
				continue
			}
			if o == nil && (nv == nil || fmt.Sprint(nv) == "" || fmt.Sprint(nv) == "0" || fmt.Sprint(nv) == "false") {
				continue // a new row: only the fields that were set
			}
			if n == nil && (ov == nil || fmt.Sprint(ov) == "") {
				continue
			}
			d[f.Name] = [2]any{shown(f, ov), shown(f, nv)}
		}
		return d
	}
	switch op {
	case "delete":
		for _, r := range before {
			write("delete", r, fieldDiff(r, nil))
		}
	default:
		for k, n := range after {
			o := old[k]
			kind := "update"
			if o == nil {
				kind = "create"
			}
			if d := fieldDiff(o, n); len(d) > 0 {
				write(kind, n, d)
			}
		}
	}
}
