package panel

import (
	"context"
	"crypto/sha256"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func noticeTerm(u *store.User, kind string) string {
	if kind == "expire_warn" {
		if u.ExpireAt == nil {
			return ""
		}
		return u.ExpireAt.UTC().Format(time.RFC3339Nano)
	}
	reset := "initial"
	if u.LastResetAt != nil {
		reset = u.LastResetAt.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%s:%d", reset, u.TrafficLimit)
}

// queueUserReminders commits scheduling markers and per-channel tasks together.
// No in-memory sink is part of this path, so overload cannot lose reminders.
func (s *Server) queueUserReminders(id string, now time.Time) error {
	// Resolve settings outside the transaction: SQLite uses one connection.
	tiers := expireTiers(s.st.GetSetting("remind_expire_days", ""))
	pctAt := atoiDefault(s.st.GetSetting("remind_usage_pct", "80"), 80)
	tgOn := s.st.GetSetting("tg_notify_users", "true") == "true"
	mailOn := s.st.GetSetting("mail_notify_users", "true") == "true"
	site, support, base := s.st.GetSetting("site_title", "Polaris"), s.st.GetSetting("sub_support_url", ""), s.subBase()
	templates := map[string]string{}
	for _, kind := range []string{"expire_warn", "traffic_warn"} {
		templates[kind] = strings.TrimSpace(s.st.GetSetting("tpl_"+kind, ""))
	}
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		lock := tx.Exec("UPDATE users SET updated_at = updated_at WHERE id = ?", id)
		if lock.Error != nil {
			return lock.Error
		}
		if lock.RowsAffected == 0 {
			return nil
		}
		var u store.User
		if err := tx.First(&u, "id = ?", id).Error; err != nil {
			return err
		}
		if u.IsService || u.Status != "active" {
			return nil
		}
		pct := float64(0)
		if u.TrafficLimit > 0 {
			pct = float64(u.Used()) * 100 / float64(u.TrafficLimit)
		}
		if u.WarnedUsage && pct < float64(pctAt) {
			if err := tx.Model(&u).Update("warned_usage", false).Error; err != nil {
				return err
			}
		}
		for _, kind := range []string{"expire_warn", "traffic_warn"} {
			if kind == "expire_warn" && !expireReminderDue(tiers, &u, now) {
				continue
			}
			if kind == "traffic_warn" && (u.TrafficLimit <= 0 || pctAt <= 0 || pct < float64(pctAt) || u.WarnedUsage) {
				continue
			}
			tier := pctAt
			if kind == "expire_warn" {
				tier = tierFor(tiers, u.ExpireAt.Sub(now))
			}
			text := ""
			if tpl := templates[kind]; tpl != "" {
				text = html.EscapeString(renderTemplate(tpl, reminderVarsWith(&u, site, base+"/sub/"+u.SubToken, support)))
			}
			if text == "" && kind == "expire_warn" {
				text = fmt.Sprintf("⏰ <b>到期提醒</b>\n%s\n账号将于 <b>%s</b> 到期（%s）\n到期后服务会自动暂停，请及时续费。", tgRule, u.ExpireAt.Local().Format("2006-01-02 15:04"), sub.ExpiryLeft(sub.Info{Expire: u.ExpireAt}))
			} else if text == "" {
				text = fmt.Sprintf("📊 <b>流量提醒</b>\n%s\n已使用 <b>%.0f%%</b>（%s / %s）\n剩余 <b>%s</b>\n%s", tgRule, pct, sub.HumanBytes(u.Used()), sub.HumanBytes(u.TrafficLimit), sub.HumanBytes(max(u.TrafficLimit-u.Used(), 0)), resetHint(&u))
			}
			recipients := map[string]string{}
			if u.TelegramID != 0 && tgOn {
				recipients["telegram"] = strconv.FormatInt(u.TelegramID, 10)
			}
			if u.Email != "" && mailOn {
				recipients["email"] = u.Email
			}
			if len(recipients) == 0 {
				continue
			}
			for channel, recipient := range recipients {
				term := noticeTerm(&u, kind)
				key := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s", id, kind, term, tier, channel, recipient)
				j := store.UserNotice{ID: fmt.Sprintf("%x", sha256.Sum256([]byte(key))), UserID: id, Kind: kind, Channel: channel, Recipient: s.st.Box.Seal(recipient), Text: s.st.Box.Seal(text), Term: term, Tier: tier, Status: "pending", NextAt: now, CreatedAt: now, UpdatedAt: now}
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&j).Error; err != nil {
					return err
				}
			}
			column, value := "warned_usage", any(true)
			if kind == "expire_warn" {
				column, value = "warned_expire", now
			}
			if err := tx.Model(&u).Update(column, value).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Server) retryUserNotices() {
	if !s.userNoticeBusy.CompareAndSwap(false, true) {
		return
	}
	defer s.userNoticeBusy.Store(false)
	var jobs []store.UserNotice
	now := time.Now()
	if err := s.st.DB.Where("status = ? AND next_at <= ? AND lease_until <= ?", "pending", now, now).Order("next_at").Limit(50).Find(&jobs).Error; err != nil {
		return
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := offset; i < len(jobs); i += 4 {
				if !s.isLeader() {
					return
				}
				s.deliverUserNotice(jobs[i].ID)
			}
		}(worker)
	}
	wg.Wait()
}

func (s *Server) deliverUserNotice(id string) {
	now, token := time.Now(), store.NewID()
	r := s.st.DB.Model(&store.UserNotice{}).Where("id = ? AND status = ? AND next_at <= ? AND lease_until <= ?", id, "pending", now, now).
		Updates(map[string]any{"lease_token": token, "lease_until": now.Add(time.Minute), "attempts": gorm.Expr("attempts + 1"), "updated_at": now})
	if r.Error != nil || r.RowsAffected != 1 {
		return
	}
	var j store.UserNotice
	if s.st.DB.First(&j, "id = ? AND lease_token = ?", id, token).Error != nil {
		return
	}
	j.Recipient = s.st.Box.MustOpen(j.Recipient)
	j.Text = s.st.Box.MustOpen(j.Text)
	finish := func(status, detail string) {
		delay := time.Duration(1<<min(j.Attempts, 8)) * time.Minute
		s.st.DB.Model(&store.UserNotice{}).Where("id = ? AND lease_token = ?", id, token).
			Updates(map[string]any{"status": status, "last_error": detail, "next_at": now.Add(delay), "lease_token": "", "lease_until": time.Time{}, "updated_at": time.Now()})
	}
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", j.UserID).Error; err != nil {
		if store.IsNotFound(err) {
			finish("canceled", "用户已删除")
		} else {
			finish("pending", "读取用户失败，等待重试")
		}
		return
	}
	valid := !u.IsService && u.Status == "active" && noticeTerm(&u, j.Kind) == j.Term
	if j.Kind == "expire_warn" {
		valid = valid && u.ExpireAt != nil && u.ExpireAt.After(now) && tierFor(expireTiers(s.st.GetSetting("remind_expire_days", "")), u.ExpireAt.Sub(now)) == j.Tier
	} else {
		pctAt := atoiDefault(s.st.GetSetting("remind_usage_pct", "80"), 80)
		valid = valid && pctAt > 0 && j.Tier == pctAt && u.TrafficLimit > 0 && float64(u.Used())*100/float64(u.TrafficLimit) >= float64(pctAt)
	}
	if j.Channel == "telegram" {
		valid = valid && strconv.FormatInt(u.TelegramID, 10) == j.Recipient && s.st.GetSetting("tg_notify_users", "true") == "true"
	}
	if j.Channel == "email" {
		valid = valid && u.Email == j.Recipient && s.st.GetSetting("mail_notify_users", "true") == "true"
	}
	if !valid {
		finish("canceled", "提醒已失效或渠道已关闭")
		return
	}
	var err error
	switch j.Channel {
	case "telegram":
		if token, _ := s.tg.creds(); token == "" {
			finish("pending", "Telegram 机器人未连接，等待配置后重试")
			return
		}
		err = s.tg.send(u.TelegramID, j.Text, s.userRenewButtons(j.Kind, &u)...)
	case "email":
		if !s.mailOn() {
			finish("pending", "邮件发送配置不可用，等待修复后重试")
			return
		}
		text := plainText(j.Text) + s.userRenewLines(j.Kind)
		plain, h := s.mailBody(mailSubjects[j.Kind], text, &u)
		err = s.sendMail(context.Background(), u.Email, mailSubjects[j.Kind], plain, h)
	default:
		finish("canceled", "未知渠道")
		return
	}
	if err != nil {
		finish("pending", "发送失败，等待重试")
		return
	}
	finish("sent", "")
}

func (s *Server) listUserNotices(c *gin.Context) {
	type view struct {
		store.UserNotice
		Username string `json:"username"`
	}
	var jobs []view
	if err := s.st.DB.Table("user_notices").Select("user_notices.*, users.username").Joins("LEFT JOIN users ON users.id = user_notices.user_id").Order("user_notices.created_at desc").Limit(100).Scan(&jobs).Error; err != nil {
		dbError(c, err)
		return
	}
	if jobs == nil {
		jobs = []view{}
	}
	c.JSON(http.StatusOK, gin.H{"items": jobs})
}
