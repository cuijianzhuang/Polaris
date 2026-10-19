package panel

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Buyers whose account the shop created may pick their own username. The
// username is only a label (nodes know users by id, subscriptions by
// token), so a rename changes nothing on the nodes.

const renameCooldown = 7 * 24 * time.Hour

var selfNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,31}$`)

var reservedNames = []string{"admin", "root", "polaris", "system", "support", "service", "telegram", "test"}

// selfRenameOn reports whether buyers may rename their accounts.
func (s *Server) selfRenameOn() bool { return s.st.GetSetting("shop_user_rename", "true") == "true" }

// canRename reports whether u may rename itself now ("" = yes, else why not).
func (s *Server) canRename(u *store.User, now time.Time) string {
	switch {
	case u == nil || u.IsService || !u.SelfService:
		return "这个账号由管理员创建，用户名请联系管理员修改"
	case !s.selfRenameOn():
		return "管理员未开放修改用户名"
	case u.RenamedAt != nil && now.Sub(*u.RenamedAt) < renameCooldown:
		return "用户名 7 天内只能修改一次，下次可修改时间：" + u.RenamedAt.Add(renameCooldown).In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04")
	}
	return ""
}

// renameSelf changes u's username on the user's own request.
func (s *Server) renameSelf(u *store.User, name, via string) error {
	name = strings.TrimSpace(name)
	now := time.Now()
	if why := s.canRename(u, now); why != "" {
		return errors.New(why)
	}
	if !selfNameRE.MatchString(name) {
		return errors.New("用户名为 3–32 位字母、数字或 _ . -，以字母或数字开头")
	}
	if strings.EqualFold(name, u.Username) {
		return errors.New("新用户名和当前相同")
	}
	for _, r := range reservedNames {
		if strings.EqualFold(name, r) {
			return errors.New("这个用户名不能使用，请换一个")
		}
	}
	old := u.Username
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var n int64
		tx.Model(&store.User{}).Where("LOWER(username) = ? AND id <> ?", strings.ToLower(name), u.ID).Count(&n)
		if n > 0 {
			return errors.New("这个用户名已被使用，请换一个")
		}
		res := tx.Model(&store.User{}).Where("id = ? AND username = ?", u.ID, old).Updates(map[string]any{"username": name, "renamed_at": now})
		if res.Error != nil {
			if store.IsDuplicate(res.Error) {
				return errors.New("这个用户名已被使用，请换一个")
			}
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errors.New("账号已变更，请刷新后重试")
		}
		return nil
	})
	if err != nil {
		return err
	}
	u.Username, u.RenamedAt = name, &now
	s.audit(via+":"+old, "user.rename", name, fmt.Sprintf("%s → %s（用户自助）", old, name), "")
	s.userEvent("user.updated", u, nil)
	return nil
}
