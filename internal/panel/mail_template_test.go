package panel

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestMailTemplateContentAndEscaping(t *testing.T) {
	s := newTestServer(t)
	s.cfg.PublicURL = "https://panel.example.com"
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("site_title", `<img src=x onerror="alert(1)">`)
	s.st.SetSetting("sub_support_url", "javascript:alert(1)")
	u := &store.User{SubToken: "fixture-token"}
	title, text := `<script>alert(1)</script>`, "流量提醒\n已用 85% <3\n\n请核对订单"
	plain, body := s.mailBody(title, text, u)
	want := text + "\n\n订阅链接：" + s.subURL(u.SubToken) + "\n用户中心：" + s.portalURL() + "\n客服：javascript:alert(1)\n\n— " + s.st.GetSetting("site_title", "")
	if plain != want {
		t.Fatalf("plain-text content changed: %q", plain)
	}
	for _, forbidden := range []string{"<script>", "<img", `href="javascript:`, "ZgotmplZ"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("unsafe HTML: %s", forbidden)
		}
	}
	for _, required := range []string{"&lt;script&gt;", "&lt;img", "85% &lt;3", "请核对订单", s.subURL(u.SubToken), s.portalURL(), "javascript:alert(1)"} {
		if !strings.Contains(body, required) {
			t.Fatalf("escaped content or fallback contact missing: %s", required)
		}
	}
	s.st.SetSetting("sub_support_url", "https://support.example.com/?a=1&b=2")
	_, body = s.mailBody("订单已开通", "开通成功", nil)
	if !strings.Contains(body, `href="https://support.example.com/?a=1&amp;b=2"`) || strings.Contains(body, u.SubToken) {
		t.Fatal("support link invalid or subscription leaked into administrator mail")
	}
}

func TestMailTemplateVerificationCode(t *testing.T) {
	s := newTestServer(t)
	for _, title := range []string{"登录验证码", "绑定邮箱验证码"} {
		plain, body := s.mailCodeBody(title, "012345")
		for _, value := range []string{"012345", fmt.Sprintf("%d 分钟内有效", int(mailCodeTTL.Minutes())), "如果不是你本人操作，请忽略这封邮件。"} {
			if !strings.Contains(plain, value) || !strings.Contains(body, value) {
				t.Fatalf("verification instructions missing: %s", value)
			}
		}
		if !strings.Contains(body, title) || strings.Contains(body, "订阅链接") {
			t.Fatal("verification mail title missing or has an unrelated subscription")
		}
	}
}

func TestMailWebLink(t *testing.T) {
	for _, tc := range []struct {
		link string
		ok   bool
	}{
		{"https://example.com/help?a=1&b=2", true}, {"http://example.com/u", true},
		{"javascript:alert(1)", false}, {"data:text/html,test", false}, {"//example.com", false},
		{"https://user:password@example.com", false}, {"@support", false}, {"https://", false},
	} {
		if got := mailWebLink(tc.link); (got != "") != tc.ok {
			t.Fatalf("link validation: %q => %q", tc.link, got)
		}
	}
}
