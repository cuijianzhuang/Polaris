package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errBindingConflict = errors.New("账号已有套餐、使用记录或订单，不能直接关联，请联系管理员处理")
var errBindingIdentity = errors.New("两个账号已绑定不同的邮箱或 Telegram，请先使用原账号登录处理")
var errBindingChanged = errors.New("账号或订阅已发生变化，请重新检查后关联")
var errBindingLinkInvalid = errors.New("请填写有效的个人订阅链接")
var errBindingLinkKind = errors.New("仅支持个人订阅链接，订阅组或客户端节点链接不能用于绑定")
var errBindingLinkMissing = errors.New("未找到个人订阅，请检查链接是否完整或已重置")

// Serialize identity transfers across panel replicas. The no-op update also
// obtains a SQLite write reservation before any ownership checks are read.
func lockAccountBindings(tx *gorm.DB) error {
	row := store.Setting{Key: "account_binding_lock"}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return err
	}
	return tx.Exec("UPDATE settings SET value = value WHERE key = ?", row.Key).Error
}

func lockBindingUsers(tx *gorm.DB, ids ...string) (map[string]store.User, error) {
	sort.Strings(ids)
	users := map[string]store.User{}
	for _, id := range ids {
		if _, seen := users[id]; seen {
			continue
		}
		update := tx.Exec("UPDATE users SET updated_at = updated_at WHERE id = ? AND is_service = ?", id, false)
		if update.Error != nil {
			return nil, update.Error
		}
		var u store.User
		if err := tx.First(&u, "id = ? AND is_service = ?", id, false).Error; err != nil {
			return nil, err
		}
		users[id] = u
	}
	return users, nil
}

// Only an unused self-service registration is disposable. Keep the row for
// audit/history; never migrate orders, combine quotas or delete an account.
func bindingUnprovisioned(tx *gorm.DB, u store.User) (bool, error) {
	if !u.SelfService || u.IsService || u.Status != "expired" || u.ExpireAt == nil || !u.ExpireAt.Before(time.Now()) ||
		u.PlanID != "" || u.TrafficLimit != 0 || u.UsedUp != 0 || u.UsedDown != 0 || u.ExtraTraffic != 0 || u.ExpireAfterDays != 0 || u.RenewDays != 0 || u.FwdMax != 0 || u.FwdUsed != 0 ||
		len(u.CDNPreferred.V) != 0 || len(u.CDNPool.V) != 0 {
		return false, nil
	}
	for _, model := range []any{&store.Ticket{}, &store.UserGroup{}, &store.UserAccess{}, &store.UserNodeQuota{}, &store.UserDevice{}, &store.TrafficHourly{}, &store.TrafficDaily{}, &store.ForwardRule{}, &store.CouponUse{}} {
		var count int64
		if err := tx.Model(model).Where("user_id = ?", u.ID).Count(&count).Error; err != nil {
			return false, err
		}
		if count != 0 {
			return false, nil
		}
	}
	var count int64
	orders := tx.Model(&store.Order{}).Where("user_id = ?", u.ID)
	if u.TelegramID != 0 {
		orders = orders.Or("telegram_id = ?", u.TelegramID)
	}
	if err := orders.Count(&count).Error; err != nil {
		return false, err
	}
	if count != 0 {
		return false, nil
	}
	if err := tx.Model(&store.SubProfile{}).Where("panel_user_id = ?", u.ID).Count(&count).Error; err != nil {
		return false, err
	}
	return count == 0, nil
}

type accountBindingPlan struct{ Keep, Retire store.User }

func planAccountBinding(tx *gorm.DB, current, subscription store.User, verifiedMail ...bool) (accountBindingPlan, error) {
	if current.ID == subscription.ID {
		return accountBindingPlan{Keep: current}, nil
	}
	if current.IsService || subscription.IsService || current.Status == "disabled" || subscription.Status == "disabled" {
		return accountBindingPlan{}, errBindingConflict
	}
	if current.Email != "" && subscription.Email != "" && current.Email != subscription.Email {
		return accountBindingPlan{}, errBindingIdentity
	}
	if current.TelegramID != 0 && subscription.TelegramID != 0 && current.TelegramID != subscription.TelegramID {
		return accountBindingPlan{}, errBindingIdentity
	}
	emptyCurrent, err := bindingUnprovisioned(tx, current)
	if err != nil {
		return accountBindingPlan{}, err
	}
	emptyTarget, err := bindingUnprovisioned(tx, subscription)
	if err != nil {
		return accountBindingPlan{}, err
	}
	// A pasted subscription is not proof of ownership of its login email.
	// Only a verified mailbox or portal-to-bot challenge may import that
	// identity into an already-provisioned current account.
	if emptyTarget && len(verifiedMail) > 0 && verifiedMail[0] {
		return accountBindingPlan{Keep: current, Retire: subscription}, nil
	}
	if emptyCurrent {
		return accountBindingPlan{Keep: subscription, Retire: current}, nil
	}
	return accountBindingPlan{}, errBindingConflict
}

func (s *Server) applyAccountBinding(tx *gorm.DB, plan accountBindingPlan) (store.User, error) {
	keep, retire := plan.Keep, plan.Retire
	if retire.ID == "" {
		return keep, nil
	}
	if keep.Email == "" {
		keep.Email = retire.Email
	}
	if keep.TelegramID == 0 {
		keep.TelegramID = retire.TelegramID
	}
	// Recheck uniqueness under the transfer lock rather than stealing another
	// account's identity, including databases with historical duplicate rows.
	var count int64
	q := tx.Model(&store.User{}).Where("id NOT IN ?", []string{keep.ID, retire.ID})
	if keep.Email != "" {
		q = q.Where("email = ?", keep.Email)
		if err := q.Count(&count).Error; err != nil {
			return keep, err
		}
		if count > 0 {
			return keep, errBindingIdentity
		}
	}
	if keep.TelegramID != 0 {
		if err := tx.Model(&store.User{}).Where("id NOT IN ? AND telegram_id = ?", []string{keep.ID, retire.ID}, keep.TelegramID).Count(&count).Error; err != nil {
			return keep, err
		}
		if count > 0 {
			return keep, errBindingIdentity
		}
	}
	if err := tx.Model(&store.User{}).Where("id = ?", retire.ID).Updates(map[string]any{"email": "", "telegram_id": 0, "status": "disabled"}).Error; err != nil {
		return keep, err
	}
	if err := tx.Model(&store.User{}).Where("id = ?", keep.ID).Updates(map[string]any{"email": keep.Email, "telegram_id": keep.TelegramID}).Error; err != nil {
		return keep, err
	}
	epoch, err := lockPortalEpoch(tx, retire.ID)
	if err != nil {
		return keep, err
	}
	if err = tx.Model(&store.PortalSessionEpoch{}).Where("user_id = ?", retire.ID).Update("version", epoch.Version+1).Error; err != nil {
		return keep, err
	}
	if err = tx.Model(&store.PortalSession{}).Where("user_id = ?", retire.ID).Update("revoked", true).Error; err != nil {
		return keep, err
	}
	retire.Email, retire.TelegramID, retire.Status = "", 0, "disabled"
	if err = s.queueUserEvent(tx, "user.updated", &retire, map[string]any{"via": "account_binding", "linkedTo": keep.ID}); err != nil {
		return keep, err
	}
	if err = s.queueUserEvent(tx, "user.updated", &keep, map[string]any{"via": "account_binding"}); err != nil {
		return keep, err
	}
	return keep, nil
}

func personalSubscription(tx *gorm.DB, text string) (store.User, error) {
	var u store.User
	text = strings.TrimSpace(text)
	if len(text) > 2048 {
		return u, errBindingLinkInvalid
	}
	token := text
	if strings.Contains(text, "://") {
		parsed, err := url.Parse(text)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || !strings.HasPrefix(parsed.Path, "/sub/") {
			return u, errBindingLinkInvalid
		}
		token = strings.TrimPrefix(parsed.Path, "/sub/")
	}
	if len(token) > 64 || !subTokenRe.MatchString(token) {
		return u, errBindingLinkKind
	}
	if err := tx.First(&u, "sub_token = ? AND is_service = ?", token, false).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return u, errBindingLinkMissing
		}
		return u, err
	}
	return u, nil
}

func bindingVersion(u store.User) string {
	// Traffic collection updates UpdatedAt continuously. Only ownership and
	// the displayed account name belong in the confirmation version; fresh
	// entitlement/order checks below protect eligibility without rejecting
	// an otherwise unchanged subscription merely because it carried traffic.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%q|%q|%d|%q|%q", u.ID, u.Email, u.TelegramID, u.SubToken, u.Username)))
	return hex.EncodeToString(sum[:])
}

type portalBindingIntent struct {
	UserID        string
	SID           string
	TargetID      string
	SourceVersion string
	TargetVersion string
}

func bindingSession(tx *gorm.DB, cl *claims, u store.User) error {
	if _, err := readPortalSession(tx, cl); err != nil {
		return errPortalSessionEnded
	}
	if cl.User != u.ID || cl.SubFP != subFingerprint(u.SubToken) {
		return errPortalSessionEnded
	}
	if cl.Chat != 0 && cl.Chat == u.TelegramID {
		return nil
	}
	if cl.Chat == 0 && cl.MailFP != "" && u.Email != "" && cl.MailFP == mailFP(u.Email) {
		return nil
	}
	return errPortalSessionEnded
}

func bindingError(c *gin.Context, err error) {
	if errors.Is(err, errPortalSessionEnded) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = errBindingChanged
	}
	expected := false
	for _, domain := range []error{errBindingConflict, errBindingIdentity, errBindingChanged, errBindingLinkInvalid, errBindingLinkKind, errBindingLinkMissing} {
		if errors.Is(err, domain) {
			expected = true
			break
		}
	}
	if !expected {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "account_binding_conflict"})
}

func (s *Server) portalSubscriptionPreview(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	current := s.portalUser(c)
	if current == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	limiter := sharedLimiter{s: s.shared, name: "account-binding", perMinute: 12}
	if !limiter.Allow(current.ID) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "操作太频繁，请稍后再试"})
		return
	}
	var req struct {
		Subscription string `json:"subscription" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var plan accountBindingPlan
	var target store.User
	cl := s.portalClaims(c)
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockAccountBindings(tx); err != nil {
			return err
		}
		var err error
		target, err = personalSubscription(tx, req.Subscription)
		if err != nil {
			return err
		}
		users, err := lockBindingUsers(tx, current.ID, target.ID)
		if err != nil {
			return err
		}
		if err = bindingSession(tx, cl, users[current.ID]); err != nil {
			return err
		}
		plan, err = planAccountBinding(tx, users[current.ID], users[target.ID])
		return err
	})
	if err != nil {
		bindingError(c, err)
		return
	}
	ticket := pki.Token(32)
	intent := portalBindingIntent{UserID: current.ID, SID: cl.SID, TargetID: target.ID, SourceVersion: bindingVersion(*current), TargetVersion: bindingVersion(target)}
	if err = s.eph.put("portal-account-bind:"+ticket, intent, 10*time.Minute); err != nil {
		serverError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"ticket": ticket, "username": plan.Keep.Username, "sameAccount": current.ID == target.ID, "message": "确认关联后，邮箱和 Telegram 使用同一账号；已开通订阅的套餐、流量和连接凭据保持不变。"})
}

func (s *Server) portalSubscriptionBind(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	current := s.portalUser(c)
	if current == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req struct {
		Ticket string `json:"ticket" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	cl := s.portalClaims(c)
	var intent portalBindingIntent
	accepted := false
	s.eph.update("portal-account-bind:"+req.Ticket, &intent, func() ephAction {
		if intent.UserID != current.ID || intent.SID != cl.SID {
			return ephKeep
		}
		accepted = true
		return ephDelete
	})
	if !accepted {
		badRequest(c, errors.New("关联确认已失效，请重新检查订阅"))
		return
	}
	var kept store.User
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockAccountBindings(tx); err != nil {
			return err
		}
		users, err := lockBindingUsers(tx, current.ID, intent.TargetID)
		if err != nil {
			return err
		}
		source, target := users[current.ID], users[intent.TargetID]
		if err = bindingSession(tx, cl, source); err != nil {
			return err
		}
		if bindingVersion(source) != intent.SourceVersion || bindingVersion(target) != intent.TargetVersion {
			return errBindingChanged
		}
		plan, err := planAccountBinding(tx, source, target)
		if err != nil {
			return err
		}
		kept, err = s.applyAccountBinding(tx, plan)
		return err
	})
	if err != nil {
		bindingError(c, err)
		return
	}
	token, err := s.issuePortalTokenVia(&kept, cl.MailFP != "", c)
	if err != nil {
		serverError(c, err)
		return
	}
	s.setPortalCookie(c, token, int(portalSessionTTL.Seconds()))
	s.audit(portalActor(current, cl.MailFP != ""), "user.subscription_bound", kept.Username, "verified account association", clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "username": kept.Username})
}

// A verified email can reuse an existing account instead of registering a
// duplicate. Changing email is explicit, so the current old email need not be
// preserved; every other identity and all service data still require checks.
func planVerifiedMailBinding(tx *gorm.DB, current store.User, addr string) (accountBindingPlan, error) {
	var others []store.User
	if err := tx.Where("email = ? AND id <> ?", addr, current.ID).Limit(2).Find(&others).Error; err != nil {
		return accountBindingPlan{}, err
	}
	if len(others) == 0 {
		current.Email = addr
		return accountBindingPlan{Keep: current}, nil
	}
	if len(others) != 1 {
		return accountBindingPlan{}, errBindingIdentity
	}
	current.Email = ""
	plan, err := planAccountBinding(tx, current, others[0], true)
	if err != nil {
		return plan, fmt.Errorf("邮箱已被其他用户使用：%w", err)
	}
	plan.Keep.Email = addr
	return plan, nil
}

func (s *Server) bindTelegramSubscription(chat int64, targetID string) (store.User, error) {
	var kept store.User
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockAccountBindings(tx); err != nil {
			return err
		}
		var existing []store.User
		if err := tx.Where("telegram_id = ?", chat).Limit(2).Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) > 1 {
			return errBindingIdentity
		}
		ids := []string{targetID}
		if len(existing) == 1 {
			ids = append(ids, existing[0].ID)
		}
		users, err := lockBindingUsers(tx, ids...)
		if err != nil {
			return err
		}
		target := users[targetID]
		if target.Status == "disabled" {
			return errBindingConflict
		}
		if target.TelegramID != 0 && target.TelegramID != chat {
			return errBindingIdentity
		}
		if len(existing) == 0 || existing[0].ID == targetID {
			if err := tx.Model(&store.User{}).Where("id = ?", targetID).Update("telegram_id", chat).Error; err != nil {
				return err
			}
			target.TelegramID = chat
			kept = target
			return s.queueUserEvent(tx, "user.updated", &kept, map[string]any{"via": "telegram_binding"})
		}
		plan, err := planAccountBinding(tx, users[existing[0].ID], target)
		if err != nil {
			return err
		}
		kept, err = s.applyAccountBinding(tx, plan)
		return err
	})
	if err == nil {
		s.wakeBusinessEvents()
	}
	return kept, err
}

// Order insertion shares the identity-transfer barrier. A checkout that
// started before association cannot create an order for a retired account
// after the last no-orders check has already succeeded.
func createBindingAwareOrder(tx *gorm.DB, o *store.Order) error {
	if err := lockAccountBindings(tx); err != nil {
		return err
	}
	if o.UserID != "" {
		users, err := lockBindingUsers(tx, o.UserID)
		if err != nil {
			return err
		}
		u := users[o.UserID]
		if u.SelfService && u.Status == "disabled" && u.Email == "" && u.TelegramID == 0 {
			return errBindingChanged
		}
	}
	return tx.Create(o).Error
}
func (s *Server) createAccountOrder(o *store.Order) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error { return createBindingAwareOrder(tx, o) })
}
