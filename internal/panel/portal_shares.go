package panel

import (
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// "Share subscriptions" in the user center (design share-subscriptions):
// the owner creates, edits, disables, resets and revokes copies of their
// subscription, each with its own token, core credentials, traffic limit
// and expiry. Revoking deletes the row — the token stops resolving and
// the share's credentials vanish from node configs on the next push, so
// a revocation never touches the main subscription or other shares (D2).

type portalShareRow struct {
	ID, Name, URL, Status, Used, Quota, Expire, LastUsed string
	Enabled                                              bool
}

func (s *Server) shareStatusText(sh *store.ShareSubscription, now time.Time) string {
	switch {
	case !sh.Enabled:
		return "已停用"
	case sh.Expired(now):
		return "已到期"
	case sh.Capped():
		return "流量已用完"
	default:
		return "正常"
	}
}

func shareTimeText(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Format("2006-01-02 15:04")
}

// portalShares is GET /u/shares.
func (s *Server) portalShares(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalUser(c)
	if u == nil {
		c.Redirect(http.StatusFound, portalPath)
		return
	}
	var rows []store.ShareSubscription
	s.st.DB.Where("user_id = ?", u.ID).Order("created_at, id").Find(&rows)
	now := time.Now().UTC()
	out := make([]portalShareRow, 0, len(rows))
	for i := range rows {
		sh := &rows[i]
		quota := "不限"
		if sh.TrafficLimit > 0 {
			quota = bytesText(sh.TrafficLimit)
		}
		out = append(out, portalShareRow{
			ID: sh.ID, Name: sh.Name, URL: s.subURL(sh.Token), Enabled: sh.Enabled,
			Status: s.shareStatusText(sh, now), Used: bytesText(sh.Used()), Quota: quota,
			Expire: shareTimeText(sh.ExpireAt), LastUsed: shareTimeText(sh.LastUsedAt),
		})
	}
	max := s.shareSubMax()
	data := map[string]any{
		"Title": s.subTitle(s.st.GetSetting("site_title", "Polaris")), "Rows": out,
		"Count": len(rows), "Max": max, "CanCreate": max > len(rows),
		"Closed": max == 0,
	}
	s.renderPortalSection(c, http.StatusOK, "account", "分享订阅", portalSharesTpl, data, u)
}

func (s *Server) portalShareWriter(c *gin.Context) *store.User {
	if !portalWrite(c) {
		return nil
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return nil
	}
	if !s.ticketLimit.Allow(clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "操作太频繁，请稍后再试"})
		return nil
	}
	return u
}

// ownShare loads one of the user's own shares; another user's share is
// indistinguishable from a missing one.
func (s *Server) ownShare(u *store.User, id string) (*store.ShareSubscription, error) {
	var sh store.ShareSubscription
	if err := s.st.DB.First(&sh, "id = ? AND user_id = ?", id, u.ID).Error; err != nil {
		return nil, fmt.Errorf("分享不存在")
	}
	return &sh, nil
}

type portalShareCreateReq struct {
	Name         string `json:"name"`
	TrafficLimit int64  `json:"trafficLimit"` // bytes, 0 = unlimited
	ExpireAt     string `json:"expireAt"`     // RFC3339, "" = follow the main account
}

func parseShareExpiry(v string) (*time.Time, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, fmt.Errorf("到期时间格式不正确")
	}
	t = t.UTC()
	return &t, nil
}

// portalShareCreate is POST /u/shares.
func (s *Server) portalShareCreate(c *gin.Context) {
	u := s.portalShareWriter(c)
	if u == nil {
		return
	}
	if !s.shareCreationOpen() {
		badRequest(c, fmt.Errorf("分享功能已关闭"))
		return
	}
	var req portalShareCreateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if len([]rune(name)) > 64 {
		badRequest(c, fmt.Errorf("备注名太长"))
		return
	}
	if name == "" {
		name = "未命名分享"
	}
	if req.TrafficLimit < 0 {
		badRequest(c, fmt.Errorf("流量上限不能为负数"))
		return
	}
	exp, err := parseShareExpiry(req.ExpireAt)
	if err != nil {
		badRequest(c, err)
		return
	}
	var count int64
	s.st.DB.Model(&store.ShareSubscription{}).Where("user_id = ?", u.ID).Count(&count)
	if int(count) >= s.shareSubMax() {
		badRequest(c, fmt.Errorf("分享数量已达上限（%d 份）", s.shareSubMax()))
		return
	}
	uuid, password, seed := newShareCredentials()
	sh := store.ShareSubscription{
		ID: store.NewID(), UserID: u.ID, Name: name, Token: pki.Token(32), Enabled: true,
		TrafficLimit: req.TrafficLimit, ExpireAt: exp,
		CredUUID: uuid, CredPassword: password, CredSSSeed: seed,
	}
	if err := s.st.DB.Create(&sh).Error; err != nil {
		serverError(c, err)
		return
	}
	s.audit("user:"+u.Username, "share.create", sh.Name, sh.ID, clientIP(c.Request))
	s.MarkDirty("share created", s.nodesForUsers(u.ID)...)
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": sh.ID, "url": s.subURL(sh.Token)})
}

type portalShareUpdateReq struct {
	Name         *string `json:"name"`
	TrafficLimit *int64  `json:"trafficLimit"`
	ExpireAt     *string `json:"expireAt"` // RFC3339; "" clears to follow-main
}

// portalShareUpdate is POST /u/shares/:id/update.
func (s *Server) portalShareUpdate(c *gin.Context) {
	u := s.portalShareWriter(c)
	if u == nil {
		return
	}
	sh, err := s.ownShare(u, c.Param("id"))
	if err != nil {
		badRequest(c, err)
		return
	}
	var req portalShareUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	updates := map[string]any{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len([]rune(name)) > 64 {
			badRequest(c, fmt.Errorf("备注名不正确"))
			return
		}
		updates["name"] = name
	}
	if req.TrafficLimit != nil {
		if *req.TrafficLimit < 0 {
			badRequest(c, fmt.Errorf("流量上限不能为负数"))
			return
		}
		updates["traffic_limit"] = *req.TrafficLimit
	}
	if req.ExpireAt != nil {
		exp, err := parseShareExpiry(*req.ExpireAt)
		if err != nil {
			badRequest(c, err)
			return
		}
		updates["expire_at"] = exp
	}
	if len(updates) == 0 {
		badRequest(c, fmt.Errorf("没有要修改的内容"))
		return
	}
	if err := s.st.DB.Model(&store.ShareSubscription{}).Where("id = ?", sh.ID).Updates(updates).Error; err != nil {
		serverError(c, err)
		return
	}
	s.audit("user:"+u.Username, "share.update", sh.Name, sh.ID, clientIP(c.Request))
	s.MarkDirty("share updated", s.nodesForUsers(u.ID)...)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// portalShareToggle is POST /u/shares/:id/toggle.
func (s *Server) portalShareToggle(c *gin.Context) {
	u := s.portalShareWriter(c)
	if u == nil {
		return
	}
	sh, err := s.ownShare(u, c.Param("id"))
	if err != nil {
		badRequest(c, err)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.st.DB.Model(&store.ShareSubscription{}).Where("id = ?", sh.ID).Update("enabled", req.Enabled).Error; err != nil {
		serverError(c, err)
		return
	}
	s.audit("user:"+u.Username, "share.update", sh.Name, fmt.Sprintf("%s enabled=%v", sh.ID, req.Enabled), clientIP(c.Request))
	s.MarkDirty("share toggled", s.nodesForUsers(u.ID)...)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// portalShareResetCredentials is POST /u/shares/:id/reset-credentials:
// a fresh token AND a fresh core credential set for this share only —
// the old link dies and clients already imported from it stop working,
// while the main subscription and other shares are untouched (D2).
func (s *Server) portalShareResetCredentials(c *gin.Context) {
	u := s.portalShareWriter(c)
	if u == nil {
		return
	}
	sh, err := s.ownShare(u, c.Param("id"))
	if err != nil {
		badRequest(c, err)
		return
	}
	uuid, password, seed := newShareCredentials()
	token := pki.Token(32)
	if err := s.st.DB.Model(&store.ShareSubscription{}).Where("id = ?", sh.ID).Updates(map[string]any{
		"token": token, "cred_uuid": uuid, "cred_password": password, "cred_ss_seed": seed,
	}).Error; err != nil {
		serverError(c, err)
		return
	}
	s.audit("user:"+u.Username, "share.reset_credentials", sh.Name, sh.ID, clientIP(c.Request))
	s.MarkDirty("share credentials reset", s.nodesForUsers(u.ID)...)
	c.JSON(http.StatusOK, gin.H{"ok": true, "url": s.subURL(token)})
}

// portalShareRevoke is POST /u/shares/:id/revoke: deleting the row kills
// both layers at once — the token stops resolving and the credentials
// leave every node config on the next push (D2).
func (s *Server) portalShareRevoke(c *gin.Context) {
	u := s.portalShareWriter(c)
	if u == nil {
		return
	}
	sh, err := s.ownShare(u, c.Param("id"))
	if err != nil {
		badRequest(c, err)
		return
	}
	if err := s.st.DB.Delete(&store.ShareSubscription{}, "id = ?", sh.ID).Error; err != nil {
		serverError(c, err)
		return
	}
	s.audit("user:"+u.Username, "share.revoke", sh.Name, sh.ID, clientIP(c.Request))
	s.MarkDirty("share revoked", s.nodesForUsers(u.ID)...)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

//go:embed assets/portal-shares.html
var portalSharesHTML string

var portalSharesTpl = template.Must(template.New("shares").Parse(portalSharesHTML))
