package panel

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var adminRoles = []string{"owner", "operator", "viewer", "support", "finance"}

// Empty roles from existing databases/backups retain their original authority.
func normalizedAdminRole(role string) string {
	if role == "" {
		return "owner"
	}
	return role
}

var selfAccountRoutes = map[string]bool{
	"GET /auth/me": true, "POST /auth/logout": true, "POST /auth/sudo": true, "POST /auth/password": true,
	"GET /auth/sessions": true, "DELETE /auth/sessions/:id": true, "POST /auth/sessions/revoke-others": true,
	"GET /auth/passkeys": true, "POST /auth/passkeys/register/begin": true, "POST /auth/passkeys/register/finish": true,
	"DELETE /auth/passkeys/:pid": true, "POST /auth/account-recovery-codes": true,
	"POST /auth/totp/setup": true, "POST /auth/totp/enable": true, "POST /auth/totp/recovery-codes": true,
	"GET /auth/github": true, "POST /auth/github/bind-url": true, "POST /auth/github/unbind": true,
	"GET /auth/sso/bindings": true, "POST /auth/sso/:id/bind-url": true, "DELETE /auth/sso/bindings/:id": true,
}

var supportRoutes = map[string]bool{
	"GET /staff/users": true, "GET /tickets": true, "GET /tickets/:id": true,
	"POST /tickets/:id/reply": true, "POST /tickets/:id/close": true, "POST /tickets/:id/reopen": true,
}

func (s *Server) adminRoleGuard(c *gin.Context) {
	// API tokens and Telegram operators keep their existing, separate rules.
	if apiTokenOf(c) != nil {
		c.Next()
		return
	}
	role := c.GetString("adminRole")
	if role == "owner" {
		c.Next()
		return
	}
	route := c.Request.Method + " " + strings.TrimPrefix(c.FullPath(), s.cfg.APIBase())
	allowed := selfAccountRoutes[route]
	switch role {
	case "operator":
		allowed = allowed || scopedRoutes[route]
	case "viewer":
		allowed = allowed || route == "GET /staff/nodes"
	case "support":
		allowed = allowed || supportRoutes[route]
	case "finance":
		allowed = allowed || route == "GET /staff/orders" || route == "GET /staff/costs"
	}
	if !allowed {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "当前后台角色无权访问此功能"})
		return
	}
	c.Next()
}

// All administrator mutations share a database admission lock. This protects
// the last owner across panel replicas and rechecks the actor after waiting.
func (s *Server) adminAccessTransaction(c *gin.Context, fn func(*gorm.DB) error) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&store.ClusterLease{Name: "admin-access-admission"}).Error; err != nil {
			return err
		}
		if err := tx.Exec("UPDATE cluster_leases SET holder = holder WHERE name = ?", "admin-access-admission").Error; err != nil {
			return err
		}
		if v, ok := c.Get("tgAdmin"); ok {
			var actor store.TGAdmin
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&actor, "id = ?", v.(*store.TGAdmin).ID).Error; err != nil || actor.Role != "owner" {
				return errors.New("管理员权限已变化，请重新登录")
			}
		} else {
			var actor store.Admin
			if err := tx.First(&actor, "id = ?", c.GetString("adminID")).Error; err != nil || normalizedAdminRole(actor.Role) != "owner" || actor.TokenVersion != c.GetInt("tokenVersion") {
				return errors.New("管理员权限已变化，请重新登录")
			}
		}
		return fn(tx)
	})
}

type adminAccessReq struct {
	Username string   `json:"username"`
	Password string   `json:"password"`
	Role     string   `json:"role"`
	NodeIDs  []string `json:"nodeIds"`
}

var adminUsernameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,32}$`)

func validateAdminAccess(tx *gorm.DB, req *adminAccessReq) error {
	if !slices.Contains(adminRoles, req.Role) {
		return commandInputError{"请选择有效的后台角色"}
	}
	if len(req.NodeIDs) > 200 {
		return commandInputError{"分配节点最多 200 个"}
	}
	slices.Sort(req.NodeIDs)
	req.NodeIDs = slices.Compact(req.NodeIDs)
	if req.Role == "operator" || req.Role == "viewer" {
		if len(req.NodeIDs) == 0 {
			return commandInputError{"节点角色必须分配至少一个节点"}
		}
		var n int64
		if err := tx.Model(&store.Node{}).Where("id IN ?", req.NodeIDs).Count(&n).Error; err != nil {
			return err
		}
		if n != int64(len(req.NodeIDs)) {
			return commandInputError{"分配节点不存在"}
		}
	} else if len(req.NodeIDs) > 0 {
		return commandInputError{"此角色不使用节点范围"}
	}
	return nil
}

func (s *Server) listAdmins(c *gin.Context) {
	var rows []store.Admin
	if err := s.st.DB.Order("created_at, id").Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	if rows == nil {
		rows = []store.Admin{}
	}
	for i := range rows {
		rows[i].Role = normalizedAdminRole(rows[i].Role)
	}
	c.JSON(http.StatusOK, rows)
}

func (s *Server) createAdmin(c *gin.Context) {
	var req adminAccessReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if !adminUsernameRE.MatchString(req.Username) {
		badRequest(c, errors.New("用户名需为 3–32 位字母、数字或 _ . -"))
		return
	}
	if len(req.Password) < 12 || len(req.Password) > 72 || strings.TrimSpace(req.Password) != req.Password {
		badRequest(c, errors.New("初始密码需为 12–72 字节，首尾不能有空格"))
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		serverError(c, err)
		return
	}
	a := store.Admin{ID: store.NewID(), Username: req.Username, PasswordHash: string(h), Role: req.Role, NodeIDs: store.NewJSON(req.NodeIDs)}
	err = s.adminAccessTransaction(c, func(tx *gorm.DB) error {
		if err := validateAdminAccess(tx, &req); err != nil {
			return err
		}
		a.NodeIDs = store.NewJSON(req.NodeIDs)
		return tx.Create(&a).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "admin.create", a.Username, "role="+a.Role)
	c.JSON(http.StatusCreated, a)
}

func (s *Server) updateAdminAccess(c *gin.Context) {
	var req adminAccessReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Password != "" || req.Username != "" {
		badRequest(c, errors.New("此接口只修改角色与节点范围"))
		return
	}
	var updated store.Admin
	err := s.adminAccessTransaction(c, func(tx *gorm.DB) error {
		if err := validateAdminAccess(tx, &req); err != nil {
			return err
		}
		if err := tx.First(&updated, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		if normalizedAdminRole(updated.Role) == "owner" && req.Role != "owner" {
			var n int64
			if err := tx.Model(&store.Admin{}).Where("role IS NULL OR role IN ?", []string{"", "owner"}).Count(&n).Error; err != nil {
				return err
			}
			if n <= 1 {
				return commandInputError{"必须保留至少一个超级管理员"}
			}
		}
		if normalizedAdminRole(updated.Role) == req.Role && slices.Equal(updated.NodeIDs.V, req.NodeIDs) {
			return nil
		}
		if err := tx.Model(&updated).Updates(map[string]any{"role": req.Role, "node_ids": store.NewJSON(req.NodeIDs), "token_version": gorm.Expr("token_version + 1")}).Error; err != nil {
			return err
		}
		return tx.Model(&store.AdminSession{}).Where("admin_id = ?", updated.ID).Update("revoked", true).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "admin.access", updated.Username, fmt.Sprintf("role=%s nodes=%v", req.Role, req.NodeIDs))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) deleteAdmin(c *gin.Context) {
	if c.Param("id") == c.GetString("adminID") {
		badRequest(c, errors.New("不能删除当前登录账号"))
		return
	}
	var a store.Admin
	err := s.adminAccessTransaction(c, func(tx *gorm.DB) error {
		if err := tx.First(&a, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		if normalizedAdminRole(a.Role) == "owner" {
			var n int64
			if err := tx.Model(&store.Admin{}).Where("role IS NULL OR role IN ?", []string{"", "owner"}).Count(&n).Error; err != nil {
				return err
			}
			if n <= 1 {
				return commandInputError{"必须保留至少一个超级管理员"}
			}
		}
		for _, m := range []any{&store.AdminSession{}, &store.Passkey{}, &store.AccountRecoveryCode{}, &store.SSOBinding{}} {
			if err := tx.Where("admin_id = ?", a.ID).Delete(m).Error; err != nil {
				return err
			}
		}
		return tx.Delete(&a).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "admin.delete", a.Username, "")
	c.Status(http.StatusNoContent)
}

type staffUser struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	Status       string     `json:"status"`
	TrafficLimit int64      `json:"trafficLimit"`
	UsedUp       int64      `json:"usedUp"`
	UsedDown     int64      `json:"usedDown"`
	ExpireAt     *time.Time `json:"expireAt"`
}

func (s *Server) staffUsers(c *gin.Context) {
	q := s.st.DB.Model(&store.User{})
	if text := strings.TrimSpace(c.Query("q")); text != "" {
		if len(text) > 128 {
			badRequest(c, errors.New("搜索内容过长"))
			return
		}
		q = q.Where(s.st.Like("username LIKE ?"), "%"+text+"%")
	}
	var rows []staffUser
	if err := q.Select("id,username,status,traffic_limit,used_up,used_down,expire_at").Order("username").Limit(100).Offset(max(0, atoiDefault(c.Query("offset"), 0))).Scan(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	if rows == nil {
		rows = []staffUser{}
	}
	c.JSON(http.StatusOK, rows)
}

func (s *Server) staffNodes(c *gin.Context) {
	a, ok := c.Get("staffAdmin")
	if !ok {
		c.JSON(http.StatusOK, []any{})
		return
	}
	var rows []store.Node
	if err := s.st.DB.Where("id IN ?", a.(*store.Admin).NodeIDs.V).Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	// Explicit response projection: never expose connection addresses, raw
	// configuration, log/error strings, credentials or customer traffic details.
	out := []gin.H{}
	for _, n := range rows {
		out = append(out, gin.H{"id": n.ID, "name": n.Name, "status": n.Status, "region": n.Region, "cpu": n.System.V.CPU, "mem": n.System.V.Mem, "uptime": n.System.V.Uptime, "lastSeen": n.LastSeen})
	}
	c.JSON(http.StatusOK, out)
}

type staffOrder struct {
	ID        string     `json:"id"`
	PlanName  string     `json:"planName"`
	Amount    string     `json:"amount"`
	Currency  string     `json:"currency"`
	Method    string     `json:"method"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	PaidAt    *time.Time `json:"paidAt"`
}

func (s *Server) staffOrders(c *gin.Context) {
	q := s.st.DB.Model(&store.Order{})
	if text := strings.TrimSpace(c.Query("q")); text != "" {
		q = q.Where("id = ?", text)
	}
	var rows []staffOrder
	if err := q.Select("id,plan_name,amount,currency,method,status,created_at,paid_at").Order("created_at desc").Limit(100).Offset(max(0, atoiDefault(c.Query("offset"), 0))).Scan(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	if rows == nil {
		rows = []staffOrder{}
	}
	c.JSON(http.StatusOK, rows)
}

func (s *Server) staffCosts(c *gin.Context) {
	var rows []struct {
		NodeID   string     `json:"nodeId"`
		Name     string     `json:"name"`
		Amount   float64    `json:"amount"`
		Currency string     `json:"currency"`
		Cycle    string     `json:"cycle"`
		ExpireAt *time.Time `json:"expireAt"`
	}
	if err := s.st.DB.Table("node_costs").Select("node_costs.node_id,nodes.name,node_costs.amount,node_costs.currency,node_costs.cycle,node_costs.expire_at").Joins("LEFT JOIN nodes ON nodes.id = node_costs.node_id").Order("nodes.name").Scan(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	if rows == nil {
		c.JSON(http.StatusOK, []any{})
		return
	}
	c.JSON(http.StatusOK, rows)
}

func (s *Server) resetStaffPassword(c *gin.Context) {
	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if len(req.Password) < 12 || len(req.Password) > 72 || strings.TrimSpace(req.Password) != req.Password {
		badRequest(c, errors.New("初始密码需为 12–72 字节，首尾不能有空格"))
		return
	}
	if c.Param("id") == c.GetString("adminID") {
		badRequest(c, errors.New("请在我的账号中修改自己的密码"))
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		serverError(c, err)
		return
	}
	var a store.Admin
	err = s.adminAccessTransaction(c, func(tx *gorm.DB) error {
		if err := tx.First(&a, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		if err := tx.Model(&a).Updates(map[string]any{"password_hash": string(h), "token_version": gorm.Expr("token_version + 1"), "failed_attempts": 0, "locked_until": nil}).Error; err != nil {
			return err
		}
		return tx.Model(&store.AdminSession{}).Where("admin_id = ?", a.ID).Update("revoked", true).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "admin.password_reset", a.Username, "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
