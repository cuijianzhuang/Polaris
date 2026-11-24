package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Passkeys for registered users — see
// docs/design/user-identity-oauth-passkey.md (batch 3a, D1–D3, D7).
// The ceremony skeleton is the admins' (passkeys.go) moved over
// unchanged in posture: challenges are single-use and short-lived,
// clone warnings reject, failures feed the shared IP ban pool. What
// differs is everything that touches identity: credentials live in
// their own table (store.UserPasskey), registration is a binding
// action behind a live session plus the binding second factor (never
// sudo — members never pass sudo), and each face derives its relying
// party from its own configured URL, never from the request.

const (
	passkeyFacePanel  = "panel"
	passkeyFacePortal = "portal"
)

// userPasskeyUser adapts a store.User to webauthn.User, mirroring
// passkeyUser: the handle is the row id's bytes, meaningful only
// inside the user table's own lookups.
type userPasskeyUser struct {
	user        store.User
	credentials []webauthn.Credential
}

func (u *userPasskeyUser) WebAuthnID() []byte                         { return []byte(u.user.ID) }
func (u *userPasskeyUser) WebAuthnName() string                       { return u.user.Username }
func (u *userPasskeyUser) WebAuthnDisplayName() string                { return u.user.Username }
func (u *userPasskeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// userWebAuthn derives the relying party for one face. The panel
// face reuses the admins' RP (the configured PublicURL); the user
// center gets its own from the portal_public_url setting, under the
// same derivation rules (webAuthnFromURL). No setting, no RP: the
// face's passkey surface simply does not exist.
func (s *Server) userWebAuthn(face string) (*webauthn.WebAuthn, string, error) {
	if face == passkeyFacePortal {
		raw := strings.TrimSpace(s.st.GetSetting("portal_public_url", ""))
		if raw == "" {
			return nil, "", errors.New("管理员尚未配置用户中心公开地址，通行密钥暂不可用")
		}
		return webAuthnFromURL(raw)
	}
	return s.webAuthn()
}

func (s *Server) userPasskeyAvailable(face string) bool {
	_, _, err := s.userWebAuthn(face)
	return err == nil
}

// memberPasskeyLoginAvailable is what /info reports for the member
// passkey button: member admission plus a working panel RP.
func (s *Server) memberPasskeyLoginAvailable() bool {
	return s.memberLoginEnabled() && s.passkeysAvailable()
}

func (s *Server) readUserPasskey(row store.UserPasskey) (webauthn.Credential, error) {
	var cred webauthn.Credential
	raw, err := s.st.Box.Open(row.Data)
	if err != nil {
		return cred, err
	}
	err = json.Unmarshal([]byte(raw), &cred)
	if err == nil && passkeyHash(cred.ID) != row.CredentialHash {
		err = errors.New("credential lookup mismatch")
	}
	return cred, err
}

func (s *Server) userPasskeyUserOn(db *gorm.DB, id, rpid string) (*userPasskeyUser, error) {
	u := &userPasskeyUser{}
	if err := db.First(&u.user, "id = ? AND is_service = ?", id, false).Error; err != nil {
		return nil, err
	}
	var rows []store.UserPasskey
	if err := db.Where("user_id = ? AND rp_id = ?", id, rpid).Order("created_at").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		cred, err := s.readUserPasskey(r)
		if err != nil {
			return nil, err
		}
		u.credentials = append(u.credentials, cred)
	}
	return u, nil
}

// --- small lookups shared by the session and binding plumbing ---

func (s *Server) userPasskeyCount(userID string) int {
	var n int64
	s.st.DB.Model(&store.UserPasskey{}).Where("user_id = ?", userID).Count(&n)
	return int(n)
}

func (s *Server) userPasskeyHashes(userID string) []string {
	var hashes []string
	s.st.DB.Model(&store.UserPasskey{}).Where("user_id = ?", userID).Pluck("credential_hash", &hashes)
	return hashes
}

// userPasskeyFPExists reports whether the user still holds the key a
// portal session's PkFP names.
func (s *Server) userPasskeyFPExists(userID, fp string) bool {
	for _, h := range s.userPasskeyHashes(userID) {
		if pkFP(h) == fp {
			return true
		}
	}
	return false
}

// userPasskeyView is one key as the binding views render it: metadata
// only — Data and CredentialHash never leave the server.
type userPasskeyView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	RPID       string     `json:"rpId"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

func (s *Server) userPasskeyViews(userID string) []userPasskeyView {
	var rows []store.UserPasskey
	s.st.DB.Where("user_id = ?", userID).Order("created_at").Find(&rows)
	out := make([]userPasskeyView, 0, len(rows))
	for _, r := range rows {
		out = append(out, userPasskeyView{ID: r.ID, Name: r.Name, RPID: r.RPID, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt})
	}
	return out
}

// --- challenges ---

// userPasskeyChallenge is the user ceremony's in-flight state. A
// registration challenge is bound to the user, the session that began
// it and the portal generation at that moment — an epoch bump (any
// credential change) voids it, like the admins' TokenVersion binding.
type userPasskeyChallenge struct {
	Kind      string
	Face      string
	UserID    string
	SID       string
	Epoch     uint64
	Name      string
	Origin    string
	Session   webauthn.SessionData
	CreatedAt time.Time
}

func (s *Server) newUserPasskeyChallenge(ch userPasskeyChallenge) (string, error) {
	id := pki.Token(32)
	ch.CreatedAt = time.Now()
	return id, s.eph.put("userpasskey:"+ch.Kind+":"+id, ch, passkeyTTL)
}

func (s *Server) takeUserPasskeyChallenge(c *gin.Context, kind string) (finishPasskeyRequest, userPasskeyChallenge, bool) {
	var req finishPasskeyRequest
	var ch userPasskeyChallenge
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return req, ch, false
	}
	if len(req.ChallengeID) > 128 || !s.eph.take("userpasskey:"+kind+":"+req.ChallengeID, &ch) || ch.Kind != kind || time.Since(ch.CreatedAt) > passkeyTTL {
		c.JSON(http.StatusBadRequest, gin.H{"error": "挑战已使用或已过期，请重试"})
		return req, ch, false
	}
	return req, ch, true
}

// --- registration (a binding action on both faces) ---

type passkeyRegRequest struct {
	Name            string `json:"name"`
	CurrentPassword string `json:"currentPassword"`
}

// userPasskeyRegBegin runs the shared half of registration begin:
// second factor, RP, per-account cap, challenge minting. The caller
// resolved the session (sid + epoch) on its own face.
func (s *Server) userPasskeyRegBegin(c *gin.Context, face string, u *store.User, sid string, epoch uint64, req passkeyRegRequest) {
	if !s.shared.allow("user-passkey-register", 10, u.ID) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请稍后重试"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 64 {
		badRequest(c, errors.New("请输入通行密钥名称（最多 64 字节）"))
		return
	}
	if err := checkBindingSecondFactor(u, req.CurrentPassword); err != nil {
		s.recordIPFailure(clientIP(c.Request), "注册通行密钥时密码错误")
		badRequest(c, err)
		return
	}
	w, origin, err := s.userWebAuthn(face)
	if err != nil {
		badRequest(c, err)
		return
	}
	pu, err := s.userPasskeyUserOn(s.st.DB, u.ID, w.Config.RPID)
	if err != nil {
		serverError(c, err)
		return
	}
	if len(pu.credentials) >= 20 {
		badRequest(c, errors.New("每个账号最多注册 20 个通行密钥"))
		return
	}
	opts, session, err := w.BeginRegistration(pu)
	if err != nil {
		serverError(c, err)
		return
	}
	id, err := s.newUserPasskeyChallenge(userPasskeyChallenge{Kind: "register", Face: face, UserID: u.ID, SID: sid, Epoch: epoch, Name: name, Origin: origin, Session: *session})
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"challengeId": id, "options": opts})
}

// userPasskeyRegFinish completes registration. verifySession
// re-proves, inside the write transaction, that the session that
// began the ceremony is still alive on its face; the epoch is
// re-checked against the challenge for both faces alike.
func (s *Server) userPasskeyRegFinish(c *gin.Context, face string, u *store.User, sid, actor string, verifySession func(tx *gorm.DB) error) {
	req, ch, ok := s.takeUserPasskeyChallenge(c, "register")
	if !ok {
		return
	}
	w, origin, err := s.userWebAuthn(face)
	if err != nil || ch.Origin != origin || ch.Face != face || ch.UserID != u.ID || ch.SID != sid {
		badRequest(c, errors.New("注册会话已变化，请重新注册"))
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(req.Credential))
	if err != nil {
		badRequest(c, errors.New("无效的通行密钥响应"))
		return
	}
	row := store.UserPasskey{ID: store.NewID(), UserID: u.ID, Name: ch.Name, RPID: w.Config.RPID}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := reservePasskeys(tx); err != nil {
			return err
		}
		if err := verifySession(tx); err != nil {
			return err
		}
		epoch, err := lockPortalEpoch(tx, u.ID)
		if err != nil {
			return err
		}
		if epoch.Version != ch.Epoch {
			return errors.New("账号已变化，请重新注册")
		}
		pu, err := s.userPasskeyUserOn(tx, u.ID, w.Config.RPID)
		if err != nil {
			return err
		}
		if pu.user.Status == "disabled" || len(pu.credentials) >= 20 {
			return errors.New("账号已变化或密钥数量已达上限")
		}
		cred, err := w.CreateCredential(pu, ch.Session, parsed)
		if err != nil {
			return errors.New("通行密钥验证失败，请重新注册")
		}
		raw, err := json.Marshal(cred)
		if err != nil {
			return err
		}
		row.CredentialHash, row.Data = passkeyHash(cred.ID), s.st.Box.Seal(string(raw))
		return tx.Create(&row).Error
	})
	if err != nil {
		badRequest(c, err)
		return
	}
	s.audit(actor, "user.passkey_registered", u.Username, row.Name, clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusCreated, gin.H{"passkey": row})
}

// lockMemberPasskeySession is the panel face's in-transaction session
// re-check: the member's AdminSession row must still be live. (The
// admins' version also demands a recent sudo; members never sudo, so
// the live session plus the begin-time second factor is the bar.)
func lockMemberPasskeySession(tx *gorm.DB, sid, userID string) error {
	var sess store.AdminSession
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&sess, "id = ? AND admin_id = ? AND revoked = ?", sid, userID, false).Error; err != nil {
		return errors.New("登录会话已失效")
	}
	if sess.ExpiresAt.Before(time.Now()) {
		return errors.New("登录会话已失效")
	}
	return nil
}

// memberPasskeyRegisterBegin: POST /auth/passkeys/user/register/begin.
func (s *Server) memberPasskeyRegisterBegin(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可管理通行密钥"})
		return
	}
	var req passkeyRegRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	epoch, ok := s.currentPortalEpoch(u.ID)
	if !ok {
		badRequest(c, errors.New("登录会话已失效"))
		return
	}
	s.userPasskeyRegBegin(c, passkeyFacePanel, u, c.GetString("sid"), epoch, req)
}

// memberPasskeyRegisterFinish: POST /auth/passkeys/user/register/finish.
func (s *Server) memberPasskeyRegisterFinish(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可管理通行密钥"})
		return
	}
	sid := c.GetString("sid")
	s.userPasskeyRegFinish(c, passkeyFacePanel, u, sid, memberActor(u), func(tx *gorm.DB) error {
		return lockMemberPasskeySession(tx, sid, u.ID)
	})
}

// portalPasskeyRegisterBegin: POST /u/passkeys/register/begin.
func (s *Server) portalPasskeyRegisterBegin(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req passkeyRegRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	cl := s.portalClaims(c)
	s.userPasskeyRegBegin(c, passkeyFacePortal, u, cl.SID, cl.PortalEpoch, req)
}

// portalPasskeyRegisterFinish: POST /u/passkeys/register/finish.
func (s *Server) portalPasskeyRegisterFinish(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	cl := s.portalClaims(c)
	s.userPasskeyRegFinish(c, passkeyFacePortal, u, cl.SID, s.portalActorFor(c, u), func(tx *gorm.DB) error {
		if _, err := readPortalSession(tx, cl); err != nil {
			return errors.New("登录会话已失效")
		}
		return nil
	})
}

// --- removal and rename ---

// deleteUserPasskey removes one key under the shared red line: the
// last key may not go when passkeys are the account's only sign-in
// method. Removal fires the invalidation pair (epoch bump +
// member-session revocation) and revokes the panel sessions the key
// itself issued, via their passkey_id stamp.
func (s *Server) deleteUserPasskey(u *store.User, keyID string) (string, error) {
	var row store.UserPasskey
	if err := s.st.DB.First(&row, "id = ? AND user_id = ?", keyID, u.ID).Error; err != nil {
		return "", errors.New("通行密钥不存在")
	}
	if s.userPasskeyCount(u.ID) <= 1 && s.userLoginMethodCount(u) <= 1 {
		return "", errLastLoginMethod
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := bumpUserEpochTx(tx, u.ID); err != nil {
			return err
		}
		if err := tx.Model(&store.AdminSession{}).Where("passkey_id = ?", row.ID).Update("revoked", true).Error; err != nil {
			return err
		}
		return tx.Delete(&row).Error
	})
	if err != nil {
		return "", err
	}
	s.revokeMemberSessions(u.ID)
	return row.Name, nil
}

func (s *Server) renameUserPasskey(u *store.User, keyID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return errors.New("请输入通行密钥名称（最多 64 字节）")
	}
	res := s.st.DB.Model(&store.UserPasskey{}).Where("id = ? AND user_id = ?", keyID, u.ID).Update("name", name)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("通行密钥不存在")
	}
	return nil
}

type passkeyDeleteRequest struct {
	ID              string `json:"id" binding:"required"`
	CurrentPassword string `json:"currentPassword"`
}

type passkeyRenameRequest struct {
	ID   string `json:"id" binding:"required"`
	Name string `json:"name"`
}

// userPasskeyDelete is the shared removal handler body; the faces
// differ only in how the principal and actor are resolved.
func (s *Server) userPasskeyDelete(c *gin.Context, u *store.User, actor string) {
	var req passkeyDeleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := checkBindingSecondFactor(u, req.CurrentPassword); err != nil {
		s.recordIPFailure(clientIP(c.Request), "删除通行密钥时密码错误")
		badRequest(c, err)
		return
	}
	name, err := s.deleteUserPasskey(u, req.ID)
	if err != nil {
		bindingRemovalError(c, err)
		return
	}
	s.audit(actor, "user.passkey_revoked", u.Username, name, clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "relogin": true})
}

func (s *Server) userPasskeyRename(c *gin.Context, u *store.User, actor string) {
	var req passkeyRenameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.renameUserPasskey(u, req.ID, req.Name); err != nil {
		badRequest(c, err)
		return
	}
	s.audit(actor, "user.passkey_renamed", u.Username, strings.TrimSpace(req.Name), clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// memberPasskeyDelete: POST /auth/passkeys/user/delete.
func (s *Server) memberPasskeyDelete(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可管理通行密钥"})
		return
	}
	s.userPasskeyDelete(c, u, memberActor(u))
}

// memberPasskeyRename: POST /auth/passkeys/user/rename.
func (s *Server) memberPasskeyRename(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可管理通行密钥"})
		return
	}
	s.userPasskeyRename(c, u, memberActor(u))
}

// portalPasskeyDelete: POST /u/passkeys/delete.
func (s *Server) portalPasskeyDelete(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	s.userPasskeyDelete(c, u, s.portalActorFor(c, u))
}

// portalPasskeyRename: POST /u/passkeys/rename.
func (s *Server) portalPasskeyRename(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	s.userPasskeyRename(c, u, s.portalActorFor(c, u))
}

// --- discoverable login ---

var errUserPasskeyClone = errors.New("通行密钥计数异常，请使用其他方式登录并检查此密钥")

// validateUserPasskeyLogin runs the discoverable-login transaction:
// the credential is located by hash within this face's RP, the handle
// must name the key's owner, a disabled account fails like any other
// bad credential, a clone warning rejects, and a good login reseals
// the credential with its advanced sign count. The returned user and
// key are populated whenever the credential identified an account,
// even on failure, so the caller can audit precisely.
func (s *Server) validateUserPasskeyLogin(w *webauthn.WebAuthn, ch userPasskeyChallenge, parsed *protocol.ParsedCredentialAssertionData) (*store.User, *store.UserPasskey, error) {
	var u store.User
	var keyRow store.UserPasskey
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := reservePasskeys(tx); err != nil {
			return err
		}
		handler := func(rawID, handle []byte) (webauthn.User, error) {
			var row store.UserPasskey
			if err := tx.First(&row, "credential_hash = ? AND rp_id = ?", passkeyHash(rawID), w.Config.RPID).Error; err != nil {
				return nil, err
			}
			if !bytes.Equal(handle, []byte(row.UserID)) {
				return nil, errors.New("user handle mismatch")
			}
			pu, err := s.userPasskeyUserOn(tx, row.UserID, w.Config.RPID)
			if err != nil {
				return nil, err
			}
			if pu.user.Status == "disabled" {
				return nil, errors.New("account disabled")
			}
			u, keyRow = pu.user, row
			return pu, nil
		}
		_, cred, err := w.ValidatePasskeyLogin(handler, ch.Session, parsed)
		if err != nil {
			return errors.New("通行密钥验证失败")
		}
		if cred.Authenticator.CloneWarning {
			return errUserPasskeyClone
		}
		raw, err := json.Marshal(cred)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		return tx.Model(&store.UserPasskey{}).Where("id = ?", keyRow.ID).Updates(map[string]any{"data": s.st.Box.Seal(string(raw)), "last_used_at": now}).Error
	})
	if err != nil {
		if u.ID != "" {
			return &u, &keyRow, err
		}
		return nil, nil, err
	}
	return &u, &keyRow, nil
}

func (s *Server) userPasskeyLoginBegin(c *gin.Context, face string) {
	w, origin, err := s.userWebAuthn(face)
	if err != nil {
		badRequest(c, err)
		return
	}
	if !s.shared.allow("user-passkey-login", 10, clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请稍后重试"})
		return
	}
	opts, session, err := w.BeginDiscoverableLogin()
	if err != nil {
		serverError(c, err)
		return
	}
	id, err := s.newUserPasskeyChallenge(userPasskeyChallenge{Kind: "login", Face: face, Origin: origin, Session: *session})
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"challengeId": id, "options": opts})
}

// userPasskeyLoginFinish is the shared half of login finish: it
// answers every failure itself (one vague 401, one IP failure
// recorded, clone warnings audited) and hands the caller the
// authenticated user and key to sign a session for.
func (s *Server) userPasskeyLoginFinish(c *gin.Context, face string) (*store.User, *store.UserPasskey, bool) {
	if !s.shared.allow("user-passkey-finish", 20, clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请稍后重试"})
		return nil, nil, false
	}
	req, ch, ok := s.takeUserPasskeyChallenge(c, "login")
	if !ok {
		return nil, nil, false
	}
	w, origin, err := s.userWebAuthn(face)
	if err != nil || ch.Origin != origin || ch.Face != face {
		badRequest(c, errors.New("登录域名已变化，请重试"))
		return nil, nil, false
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(req.Credential))
	if err != nil {
		badRequest(c, errors.New("无效的通行密钥响应"))
		return nil, nil, false
	}
	u, key, err := s.validateUserPasskeyLogin(w, ch, parsed)
	if err != nil {
		if errors.Is(err, errUserPasskeyClone) && u != nil {
			s.audit("pk:"+u.Username, "user.passkey_login_failed", u.Username, "clone warning", clientIP(c.Request))
		}
		s.recordIPFailure(clientIP(c.Request), "用户通行密钥登录失败")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "通行密钥验证失败，请重试或使用其他登录方式"})
		return nil, nil, false
	}
	return u, key, true
}

// beginUserPasskeyLogin: POST /auth/passkeys/user/login/begin — the
// panel face's discoverable entry (public, behind loginGuard).
func (s *Server) beginUserPasskeyLogin(c *gin.Context) {
	if !s.memberLoginEnabled() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册用户登录面板未开启"})
		return
	}
	s.userPasskeyLoginBegin(c, passkeyFacePanel)
}

// finishUserPasskeyLogin: POST /auth/passkeys/user/login/finish.
// Success issues a member session stamped with the key's id, so
// deleting the key revokes exactly the sessions it minted (the
// admins' passkey_id linkage, D3).
func (s *Server) finishUserPasskeyLogin(c *gin.Context) {
	if !s.memberLoginEnabled() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册用户登录面板未开启"})
		return
	}
	u, key, ok := s.userPasskeyLoginFinish(c, passkeyFacePanel)
	if !ok {
		return
	}
	s.completeMemberPasskeyLogin(c, u, key)
}

// completeMemberPasskeyLogin issues the member session for a
// validated user passkey login and stamps the session with the key's
// id, so deleting the key ends exactly the sessions it minted. It
// answers its own failures; validation failures were already answered
// by userPasskeyLoginFinish (or, for the unified finish, by its own
// dispatch). Shared by the user-only finish and the unified finish
// (anylogin.go).
func (s *Server) completeMemberPasskeyLogin(c *gin.Context, u *store.User, key *store.UserPasskey) bool {
	tok, err := s.issueMemberToken(u, c)
	if err != nil {
		serverError(c, err)
		return false
	}
	claims, err := s.parseToken(tok)
	if err != nil {
		serverError(c, err)
		return false
	}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := reservePasskeys(tx); err != nil {
			return err
		}
		var sess store.AdminSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&sess, "id = ? AND admin_id = ? AND revoked = ?", claims.SID, u.ID, false).Error; err != nil {
			return err
		}
		var k store.UserPasskey
		if err := tx.First(&k, "id = ? AND user_id = ?", key.ID, u.ID).Error; err != nil {
			return err
		}
		r := tx.Model(&store.AdminSession{}).Where("id = ? AND revoked = ?", claims.SID, false).Update("passkey_id", key.ID)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return errors.New("session revoked")
		}
		return nil
	})
	if err != nil {
		s.st.DB.Model(&store.AdminSession{}).Where("id = ?", claims.SID).Update("revoked", true)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "账号或密钥已撤销，请重新登录"})
		return false
	}
	s.audit(memberActor(u), "member.login", u.Username, "passkey", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"token": tok, "username": memberDisplayName(u), "expiresIn": int(tokenTTL.Seconds())})
	return true
}

// portalPasskeyLoginBegin: POST /u/passkeys/login/begin — the user
// center's discoverable entry (no session yet, by definition).
func (s *Server) portalPasskeyLoginBegin(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	if until, banned := s.ipBannedUntil(clientIP(c.Request)); banned {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "尝试过于频繁，请在 " + until.Format("15:04") + " 后再试"})
		return
	}
	s.userPasskeyLoginBegin(c, passkeyFacePortal)
}

// portalPasskeyLoginFinish: POST /u/passkeys/login/finish.
func (s *Server) portalPasskeyLoginFinish(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	if until, banned := s.ipBannedUntil(clientIP(c.Request)); banned {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "尝试过于频繁，请在 " + until.Format("15:04") + " 后再试"})
		return
	}
	u, key, ok := s.userPasskeyLoginFinish(c, passkeyFacePortal)
	if !ok {
		return
	}
	tok, err := s.issuePortalPasskeyToken(u, key.CredentialHash, c)
	if err != nil {
		serverError(c, err)
		return
	}
	s.setPortalCookie(c, tok, int(portalSessionTTL.Seconds()))
	s.audit("pk:"+u.Username, "user.portal_login", u.Username, "passkey", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
