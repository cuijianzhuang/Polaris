//go:build !pro

package panel

import "context"

// Community build: the Telegram admin menus are a paid feature (compiled
// from polaris-pro). Alerts, sign-in and user self-service stay available.

func (b *tgBot) adminCommand(_, _ string, chat int64, id tgIdentity) {
	if id.admin == nil {
		b.send(chat, "仅管理员可用")
		return
	}
	b.send(chat, tgProText, backRow(""))
}

func (b *tgBot) adminCallback(_ context.Context, _ []string, chat, msg int64, _ tgIdentity, answer func(string)) {
	answer("管理菜单是专业版功能")
	b.edit(chat, msg, tgProText, backRow(""))
}

func (b *tgBot) adminView(string, string, tgIdentity) (string, [][]button) {
	return tgProText, [][]button{backRow("")}
}
