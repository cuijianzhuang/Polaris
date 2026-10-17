//go:build !pro

package panel

import (
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Community build: the paid features (per-user speed limits, the Telegram
// admin menus, backup push) are compiled from the polaris-pro sources with
// -tags pro. Here they are stubs: settings stay editable where harmless and
// the UI shows the features as locked.

const proBuild = false

// userLimits: no limiter in community builds, so no user is limited.
func (s *Server) userLimits(map[string]store.User) []model.UserLimit { return nil }

// pushBackup: the archive stays local.
func (s *Server) pushBackup(string, bool) map[string]string {
	if len(s.pushTargets()) == 0 {
		return map[string]string{}
	}
	return map[string]string{"license": "备份推送是专业版功能（社区版构建不包含），本地备份已保存"}
}
