package panel

import (
	"net/http"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/mailer"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"github.com/gin-gonic/gin"
)

// setAvatar changes only the current account's visual preference. This
// address is never used for authentication, recovery, SMTP or notifications.
func (s *Server) setAvatar(c *gin.Context) {
	id := c.GetString("adminID")
	var model any = &store.Admin{}
	if current, ok := c.Get("tgAdmin"); ok {
		id = current.(*store.TGAdmin).ID
		model = &store.TGAdmin{}
	}
	if id == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "当前账号不支持修改头像"})
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	email := mailer.Normalize(req.Email)
	if strings.TrimSpace(req.Email) != "" && email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写正确的邮箱地址"})
		return
	}
	if err := s.st.DB.Model(model).Where("id = ?", id).Update("avatar_email", email).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"avatarEmail": email, "avatarURL": sub.AvatarURL(email)})
}
