package panel

import (
	"crypto/subtle"
	"net/http"
	"strconv"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const portalTelegramBindPrefix = "ubind_"

func sameBindingOwner(request tgWebLogin, current *claims) bool {
	owner := request.Owner
	return owner != nil && current != nil && owner.User == current.User && owner.SID == current.SID && owner.PortalEpoch == current.PortalEpoch && owner.SubFP == current.SubFP && portalIdentity(owner) == portalIdentity(current)
}
func (s *Server) portalTelegramBindStart(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	if u.TelegramID != 0 {
		bindingError(c, errBindingIdentity)
		return
	}
	if !s.portalTelegramConfigured() || !s.tgWebLoginEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Telegram 机器人暂时未就绪，请稍后重试"})
		return
	}
	s.startWebLogin(c, &s.portalBindWeb, portalTelegramBindPrefix, s.portalClaims(c))
}
func (s *Server) portalTelegramBindCancel(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	if s.portalUser(c) == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var pending tgWebLogin
	s.eph.update(s.portalBindWeb.key(req.ID), &pending, func() ephAction {
		if !sameBindingOwner(pending, s.portalClaims(c)) || subtle.ConstantTimeCompare([]byte(req.Secret), []byte(pending.Secret)) != 1 {
			return ephKeep
		}
		return ephDelete
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
func (s *Server) portalTelegramBindPoll(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	current := s.portalUser(c)
	if current == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	if !s.portalTelegramConfigured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "管理员已关闭 Telegram 机器人"})
		return
	}
	cl := s.portalClaims(c)
	subject, chat, ok := s.pollWebLogin(c, &s.portalBindWeb, func(req tgWebLogin) bool { return sameBindingOwner(req, cl) })
	if !ok {
		return
	}
	if chat <= 0 || subject != "tg:"+strconv.FormatInt(chat, 10) {
		bindingError(c, errBindingIdentity)
		return
	}
	var kept store.User
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockAccountBindings(tx); err != nil {
			return err
		}
		var actors []store.User
		if err := tx.Where("telegram_id = ?", chat).Limit(2).Find(&actors).Error; err != nil {
			return err
		}
		if len(actors) > 1 {
			return errBindingIdentity
		}
		ids := []string{current.ID}
		if len(actors) == 1 {
			ids = append(ids, actors[0].ID)
		}
		users, err := lockBindingUsers(tx, ids...)
		if err != nil {
			return err
		}
		source := users[current.ID]
		if err = bindingSession(tx, cl, source); err != nil {
			return err
		}
		if source.TelegramID != 0 && source.TelegramID != chat {
			return errBindingIdentity
		}
		if len(actors) == 0 || actors[0].ID == current.ID {
			source.TelegramID = chat
			kept = source
			if err = tx.Model(&store.User{}).Where("id = ?", source.ID).Update("telegram_id", chat).Error; err != nil {
				return err
			}
			return s.queueUserEvent(tx, "user.updated", &kept, map[string]any{"via": "portal_telegram_binding"})
		}
		plan, err := planAccountBinding(tx, source, users[actors[0].ID], true)
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
	s.audit(portalActor(current, cl.MailFP != ""), "user.telegram_bound", kept.Username, "portal number confirmation", clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "username": kept.Username})
}

func (b *tgBot) portalTelegramBindPrompt(chat int64, id string) {
	if chat <= 0 {
		return
	}
	var pending tgWebLogin
	if !b.s.eph.get(b.s.portalBindWeb.key(id), &pending) || pending.Owner == nil {
		b.send(chat, "绑定请求已失效，请在用户中心重新发起。")
		return
	}
	var owner store.User
	if b.s.st.DB.First(&owner, "id = ? AND is_service = ?", pending.Owner.User, false).Error != nil || bindingSession(b.s.st.DB, pending.Owner, owner) != nil {
		b.send(chat, "网页会话已失效，请重新登录用户中心。")
		return
	}
	identity := "网页账号：<b>" + esc(owner.Username) + "</b>"
	if owner.Email != "" {
		identity += "\n已验证邮箱：" + esc(owner.Email)
	}
	identity += "\n将当前 Telegram 设为此账号的登录方式；已有套餐或身份冲突时不会覆盖。"
	b.loginPrompt(&b.s.portalBindWeb, chat, id, "ub", "绑定 Telegram 账号", identity, "绑定请求已失效，请在用户中心重新发起。")
}
func (b *tgBot) portalTelegramBindAnswer(chat, msg int64, id, pick string, answer func(string)) {
	result, _ := b.loginPick(&b.s.portalBindWeb, chat, id, pick, "tg:"+strconv.FormatInt(chat, 10))
	switch result {
	case "ok":
		answer("已确认绑定请求")
		b.edit(chat, msg, "✅ 已确认绑定请求，请回到网页查看关联结果。", backRow(""))
	case "denied", "wrong":
		answer("绑定已拒绝")
		b.edit(chat, msg, "🚫 已拒绝本次账号绑定。请在用户中心重新发起。", backRow(""))
	default:
		answer("绑定请求已失效")
	}
}
