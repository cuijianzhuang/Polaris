package panel

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// groupMsg builds a supergroup message update: the sender's user ID differs
// from the (negative) chat ID, unlike the private-chat helper msg().
func groupMsg(chat, from int64, text string) map[string]any {
	return map[string]any{"message": map[string]any{"message_id": 1, "from": map[string]any{"id": from, "username": "u"},
		"chat": map[string]any{"id": chat, "type": "supergroup"}, "text": text}}
}

func startTestBot(t *testing.T) (*Server, *fakeTG) {
	t.Helper()
	fake := &fakeTG{}
	api := httptest.NewServer(fake)
	t.Cleanup(api.Close)
	s := newTestServer(t)
	licenseAll(s)
	s.st.SetSetting("tg_enabled", "true")
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	s.st.SetSetting("tg_api_base", api.URL)
	s.st.SetSetting("tg_poll_timeout", "0")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.tg.run(ctx)
	waitUntil(t, "bot running", s.tg.isRunning)
	return s, fake
}

// In a group the bot serves nothing but /chatid, which echoes the group's
// own ID so the admin can aim release announcements at the group.
func TestTelegramGroupChatID(t *testing.T) {
	_, fake := startTestBot(t)
	const group = int64(-1001234567890)
	fake.push(groupMsg(group, 555, "/chatid"))
	waitUntil(t, "chatid reply", func() bool {
		return strings.Contains(fake.textsTo(group), "本群 ID：-1001234567890")
	})
	fake.push(groupMsg(group, 555, "/chatid@PolarisPanel_bot"))
	waitUntil(t, "chatid reply with bot suffix", func() bool {
		return strings.Count(fake.textsTo(group), "本群 ID") == 2
	})
	fake.push(groupMsg(group, 555, "/status"))
	waitUntil(t, "group refusal", func() bool {
		return strings.Contains(fake.textsTo(group), "请私聊机器人使用")
	})
}

// The release announcement goes to the configured group only, in its public
// wording; an unset or invalid setting posts nothing.
func TestAnnounceReleaseToGroup(t *testing.T) {
	s, fake := startTestBot(t)
	const group = int64(-1001234567890)

	s.announceReleaseToGroup("v9.9.9")
	if got := fake.textsTo(group); got != "" {
		t.Fatalf("unset setting posted to the group: %q", got)
	}
	s.st.SetSetting("update_notify_chat", "-1001234567890")
	s.announceReleaseToGroup("v9.9.9")
	got := fake.textsTo(group)
	if !strings.Contains(got, "Polaris 新版本 v9.9.9 已发布") || !strings.Contains(got, "releases/tag/v9.9.9") {
		t.Fatalf("announcement missing or malformed: %q", got)
	}
	s.st.SetSetting("update_notify_chat", "not-a-number")
	s.announceReleaseToGroup("v9.9.10")
	if strings.Contains(fake.textsTo(group), "v9.9.10") {
		t.Fatal("invalid setting posted to the group")
	}
}
