package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

type commandInputError struct{ message string }

func (e commandInputError) Error() string { return e.message }

// User commands serialize against the same user row and, when a key is
// supplied, commit the mutation, audit and replayable result atomically.
// dirty lists extra nodes to rebuild, e.g. ones the user may be leaving.
func (s *Server) userCommand(c *gin.Context, action string, apply func(*gorm.DB, *store.User, time.Time) (string, error), dirty ...string) {
	var info *idempotencyRequest
	if v, ok := c.Get(idemContextKey); ok {
		info = v.(*idempotencyRequest)
	}
	var cached *store.IdempotencyKey
	var body []byte
	var user store.User
	var detail string
	err := s.st.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var err error
		if cached, err = reserveUserCommand(tx, info); err != nil || cached != nil {
			return err
		}
		// A no-op write takes a row lock on PostgreSQL and the writer lock on
		// SQLite before any reads, including GORM's change-audit snapshots.
		lock := tx.Exec("UPDATE users SET updated_at = updated_at WHERE id = ? AND is_service = ?", c.Param("id"), false)
		if lock.Error != nil {
			return lock.Error
		}
		if lock.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		if err = tx.First(&user, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		now := time.Now()
		if detail, err = apply(tx, &user, now); err != nil {
			return err
		}
		user.Status = computeStatus(&user, now)
		if err = tx.Model(&store.User{}).Where("id = ?", user.ID).Update("status", user.Status).Error; err != nil {
			return err
		}
		if err = tx.First(&user, "id = ?", user.ID).Error; err != nil {
			return err
		}
		if body, err = json.Marshal(commandUserView(c, &user)); err != nil {
			return err
		}
		audit := store.AuditLog{Admin: c.GetString("admin"), Action: action, Target: truncate(user.Username, 250), Detail: truncate(detail, 4000), IP: clientIP(c.Request)}
		if err = tx.Create(&audit).Error; err != nil {
			return err
		}
		if info != nil {
			result := tx.Model(&store.IdempotencyKey{}).Where("key = ?", info.Key).Updates(map[string]any{"status": http.StatusOK, "body": body})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("幂等结果未能保存")
			}
		}
		return nil
	})
	if err != nil {
		var input commandInputError
		switch {
		case errors.As(err, &input):
			badRequest(c, err)
		case errors.Is(err, gorm.ErrRecordNotFound):
			notFound(c)
		default:
			idempotencyError(c, err)
		}
		return
	}
	if cached != nil {
		// A scope reduction must not replay credentials that were visible
		// when the same order originally ran with a broad token.
		if t := apiTokenOf(c); t != nil && !slices.Contains(t.Scopes.V, ScopeUsersRead) && !slices.Contains(t.Scopes.V, ScopeUsersWrite) {
			var previous store.User
			if err := json.Unmarshal(cached.Body, &previous); err != nil {
				serverError(c, err)
				return
			}
			redacted, err := json.Marshal(commandUserView(c, &previous))
			if err != nil {
				serverError(c, err)
				return
			}
			cached.Body = redacted
		}
		replayIdempotency(c, *cached)
		return
	}
	s.MarkDirty(action, append(dirty, s.nodesForUsers(user.ID)...)...)
	if action == "user.pack" {
		s.userEvent("user.traffic_added", &user, map[string]any{"detail": detail})
	}
	if action == "user.plan" {
		s.userEvent("user.plan_changed", &user, map[string]any{"plan_id": user.PlanID, "detail": detail})
	}
	if action == "user.renew" {
		s.notify(Event{Kind: "user_renewed", UserID: user.ID, Text: "🔄 用户 " + user.Username + " 已续期：" + detail})
	}
	if action == "user.add_traffic" {
		s.userEvent("user.traffic_added", &user, map[string]any{"detail": detail})
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// Narrow write scopes return an entitlement summary, not proxy passwords or
// subscription tokens. Existing users:read/users:write callers retain the
// previous User response shape.
func commandUserView(c *gin.Context, u *store.User) any {
	t := apiTokenOf(c)
	if t == nil || slices.Contains(t.Scopes.V, ScopeUsersRead) || slices.Contains(t.Scopes.V, ScopeUsersWrite) {
		return u
	}
	return gin.H{"id": u.ID, "username": u.Username, "status": u.Status, "planId": u.PlanID, "trafficLimit": u.TrafficLimit, "usedUp": u.UsedUp, "usedDown": u.UsedDown, "expireAt": u.ExpireAt, "expireAfterDays": u.ExpireAfterDays}
}

// POST /users/:id/renew: expired accounts start from now; first-use accounts
// gain days in their unstarted period; unlimited accounts remain unlimited.
func (s *Server) renewUser(c *gin.Context) {
	var req struct {
		Days         int  `json:"days" binding:"required"`
		ResetTraffic bool `json:"resetTraffic"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Days < 1 || req.Days > 3650 {
		badRequest(c, errors.New("天数应为 1–3650"))
		return
	}
	s.userCommand(c, "user.renew", func(tx *gorm.DB, u *store.User, now time.Time) (string, error) {
		upd := map[string]any{"warned_expire": nil}
		detail := ""
		switch {
		case u.ExpireAt != nil:
			from := *u.ExpireAt
			if from.Before(now) {
				from = now
			}
			exp := from.AddDate(0, 0, req.Days)
			if exp.Year() > 9999 {
				return "", commandInputError{"到期日期超出支持范围"}
			}
			upd["expire_at"], u.ExpireAt = exp, &exp
			detail = fmt.Sprintf("+%d 天，至 %s", req.Days, exp.Format("2006-01-02"))
		case u.ExpireAfterDays > 0:
			if u.ExpireAfterDays > int(^uint(0)>>1)-req.Days {
				return "", commandInputError{"有效天数超出支持范围"}
			}
			u.ExpireAfterDays += req.Days
			upd["expire_after_days"] = u.ExpireAfterDays
			detail = fmt.Sprintf("首次使用后有效期 +%d 天，共 %d 天", req.Days, u.ExpireAfterDays)
		default:
			detail = fmt.Sprintf("+%d 天（账号不限期，未改动）", req.Days)
		}
		if req.ResetTraffic {
			u.UsedUp, u.UsedDown = 0, 0
			upd["used_up"], upd["used_down"], upd["last_reset_at"], upd["warned_usage"] = 0, 0, now, false
			if err := tx.Model(&store.UserNodeQuota{}).Where("user_id = ?", u.ID).Update("used", 0).Error; err != nil {
				return "", err
			}
			detail += "，流量已重置"
		}
		return detail, tx.Model(&store.User{}).Where("id = ?", u.ID).Updates(upd).Error
	})
}

// POST /users/:id/add-traffic: add to the latest quota under the user lock.
func (s *Server) addUserTraffic(c *gin.Context) {
	var req struct {
		GB float64 `json:"gb" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.GB <= 0 || req.GB > 1<<20 || math.IsNaN(req.GB) || math.IsInf(req.GB, 0) {
		badRequest(c, errors.New("流量应为大于 0 的 GB 数"))
		return
	}
	add := int64(req.GB * (1 << 30))
	if add < 1 {
		badRequest(c, errors.New("增加的流量不能小于 1 字节"))
		return
	}
	s.userCommand(c, "user.add_traffic", func(tx *gorm.DB, u *store.User, _ time.Time) (string, error) {
		if u.TrafficLimit == 0 {
			return fmt.Sprintf("+%g GB（账号不限流量，未改动）", req.GB), nil
		}
		if u.TrafficLimit > math.MaxInt64-add {
			return "", commandInputError{"流量配额超出支持范围"}
		}
		u.TrafficLimit += add
		detail := fmt.Sprintf("+%g GB，共 %.2f GB", req.GB, float64(u.TrafficLimit)/(1<<30))
		return detail, tx.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"traffic_limit": u.TrafficLimit, "warned_usage": false}).Error
	})
}

// userJSON answers with the user as GET /users/:id would.
func (s *Server) userJSON(c *gin.Context, id string) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", id).Error; err != nil {
		dbError(c, err)
		return
	}
	c.JSON(http.StatusOK, u)
}
