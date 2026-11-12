package panel

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errPortalSessionEnded = errors.New("会话已结束，请重新登录")

func lockPortalEpoch(tx *gorm.DB, userID string) (store.PortalSessionEpoch, error) {
	epoch := store.PortalSessionEpoch{UserID: userID, Version: 1}
	// The insert is a write barrier on SQLite; FOR UPDATE serializes PostgreSQL.
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&epoch).Error; err != nil {
		return epoch, err
	}
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&epoch, "user_id = ?", userID).Error
	return epoch, err
}

// Portal sessions are independent of admin sessions and subscription devices.
func (s *Server) recordPortalSession(u *store.User, cl *claims, via string, contexts ...*gin.Context) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		epoch, err := lockPortalEpoch(tx, u.ID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		session := store.PortalSession{ID: store.NewID(), Epoch: epoch.Version, UserID: u.ID, Via: via, SubFP: cl.SubFP, IdentityFP: portalIdentity(cl), CreatedAt: now, LastSeen: now, ExpiresAt: cl.ExpiresAt.Time}
		if len(contexts) > 0 && contexts[0] != nil {
			c := contexts[0]
			session.IP = clientIP(c.Request)
			session.UserAgent = truncate(c.Request.UserAgent(), 250)
			// Credential renewal keeps the current browser's identity and cannot
			// resurrect a session that another device has just revoked.
			if cached, ok := c.Get("portalClaims"); ok {
				parent := cached.(*claims)
				if parent.User == u.ID {
					old, e := readPortalSession(tx, parent)
					if e != nil {
						return errPortalSessionEnded
					}
					session.ID, session.CreatedAt = old.ID, old.CreatedAt
					changes := map[string]any{"epoch": epoch.Version, "via": session.Via, "sub_fp": session.SubFP, "identity_fp": session.IdentityFP, "ip": session.IP, "user_agent": session.UserAgent, "last_seen": now, "expires_at": session.ExpiresAt}
					updated := tx.Model(&store.PortalSession{}).Where("id = ? AND revoked = ?", old.ID, false).Updates(changes)
					if updated.Error != nil {
						return updated.Error
					}
					if updated.RowsAffected != 1 {
						return errPortalSessionEnded
					}
					cl.SID, cl.PortalEpoch = old.ID, epoch.Version
					return nil
				}
			}
		}
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		cl.SID, cl.PortalEpoch = session.ID, epoch.Version
		return nil
	})
}

func portalIdentity(cl *claims) string {
	if cl.Chat != 0 {
		return "tg:" + strconv.FormatInt(cl.Chat, 10)
	}
	return "mail:" + cl.MailFP
}

func readPortalSession(db *gorm.DB, cl *claims) (store.PortalSession, error) {
	var session store.PortalSession
	if cl.SID == "" || cl.PortalEpoch == 0 {
		return session, errPortalSessionEnded
	}
	err := db.Model(&store.PortalSession{}).Select("portal_sessions.*").Joins("JOIN portal_session_epochs ON portal_session_epochs.user_id = portal_sessions.user_id AND portal_session_epochs.version = portal_sessions.epoch").Where("portal_sessions.id = ? AND portal_sessions.user_id = ? AND portal_sessions.epoch = ? AND portal_sessions.revoked = ? AND portal_sessions.expires_at > ?", cl.SID, cl.User, cl.PortalEpoch, false, time.Now().UTC()).First(&session).Error
	if err != nil {
		return session, err
	}
	if session.SubFP != cl.SubFP || session.IdentityFP != portalIdentity(cl) {
		return session, errPortalSessionEnded
	}
	return session, nil
}

func (s *Server) portalSessionAlive(cl *claims) bool {
	// Legacy stateless cookies require sign-in once. Never recreate rows from a cookie.
	session, err := readPortalSession(s.st.DB, cl)
	if err != nil {
		return false
	}
	if time.Since(session.LastSeen) > time.Minute {
		s.st.DB.Model(&store.PortalSession{}).Where("id = ? AND epoch = ? AND revoked = ?", session.ID, cl.PortalEpoch, false).Update("last_seen", time.Now().UTC())
	}
	return true
}

type portalSessionView struct {
	ID        string    `json:"id"`
	Via       string    `json:"via"`
	Device    string    `json:"device"`
	IP        string    `json:"ip"`
	Current   bool      `json:"current"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (s *Server) listPortalSessions(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	cl := s.portalClaims(c)
	identities := []string{}
	if u.TelegramID != 0 {
		identities = append(identities, "tg:"+strconv.FormatInt(u.TelegramID, 10))
	}
	if u.Email != "" {
		identities = append(identities, "mail:"+mailFP(u.Email))
	}
	var rows []store.PortalSession
	if err := s.st.DB.Where("user_id = ? AND epoch = ? AND revoked = ? AND expires_at > ? AND sub_fp = ? AND identity_fp IN ?", u.ID, cl.PortalEpoch, false, time.Now().UTC(), subFingerprint(u.SubToken), identities).Order("last_seen DESC, id DESC").Limit(101).Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	more := len(rows) > 100
	if more {
		rows = rows[:100]
	}
	out := make([]portalSessionView, 0, len(rows))
	for _, row := range rows {
		out = append(out, portalSessionView{ID: row.ID, Via: row.Via, Device: shortUA(row.UserAgent), IP: row.IP, Current: row.ID == cl.SID, CreatedAt: row.CreatedAt, LastSeen: row.LastSeen, ExpiresAt: row.ExpiresAt})
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "hasMore": more})
}

func (s *Server) revokePortalSession(others bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !portalWrite(c) {
			return
		}
		u := s.portalUser(c)
		if u == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
			return
		}
		cl := s.portalClaims(c)
		target := "others"
		if !others {
			var req struct {
				ID string `json:"id" binding:"required"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				badRequest(c, err)
				return
			}
			target = req.ID
		}
		var count int64
		var renewed string
		err := s.st.DB.Transaction(func(tx *gorm.DB) error {
			epoch, e := lockPortalEpoch(tx, u.ID)
			if e != nil {
				return e
			}
			if _, e := readPortalSession(tx, cl); e != nil {
				return errPortalSessionEnded
			}
			q := tx.Model(&store.PortalSession{}).Where("user_id = ? AND revoked = ?", u.ID, false)
			if others {
				q = q.Where("id <> ?", cl.SID)
			} else {
				q = q.Where("id = ?", target)
			}
			result := q.Update("revoked", true)
			if result.Error != nil {
				return result.Error
			}
			count = result.RowsAffected
			if others {
				epoch.Version++
				if e := tx.Model(&store.PortalSessionEpoch{}).Where("user_id = ?", u.ID).Update("version", epoch.Version).Error; e != nil {
					return e
				}
				if e := tx.Model(&store.PortalSession{}).Where("id = ? AND user_id = ?", cl.SID, u.ID).Update("epoch", epoch.Version).Error; e != nil {
					return e
				}
				fresh := *cl
				fresh.PortalEpoch = epoch.Version
				renewed, e = jwt.NewWithClaims(jwt.SigningMethodHS256, fresh).SignedString(s.jwtKey)
				return e
			}
			return nil
		})
		if errors.Is(err, errPortalSessionEnded) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		if err != nil {
			serverError(c, err)
			return
		}
		current := !others && target == cl.SID
		if current {
			s.setPortalCookie(c, "", -1)
		} else if renewed != "" {
			s.setPortalCookie(c, renewed, int(time.Until(cl.ExpiresAt.Time).Seconds()))
		}
		s.audit(portalActor(u, s.portalViaMail(c)), "user.session_revoke", u.Username, target, clientIP(c.Request))
		c.JSON(http.StatusOK, gin.H{"revoked": count, "current": current})
	}
}

func (s *Server) prunePortalSessions() {
	if err := s.st.DB.Where("expires_at <= ?", time.Now().UTC()).Delete(&store.PortalSession{}).Error; err != nil {
		s.log.Warn("prune portal sessions", "err", err)
	}
}
