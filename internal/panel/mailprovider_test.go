package panel

import (
	"context"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/mailer"
)

// mapGet adapts a map to the settings getter buildMailSender consumes.
func mapGet(vals map[string]string) func(string, string) string {
	return func(k, def string) string {
		if v, ok := vals[k]; ok {
			return v
		}
		return def
	}
}

func TestBuildMailSenderProviders(t *testing.T) {
	smtpVals := map[string]string{"smtp_host": "127.0.0.1", "smtp_from": "noreply@example.com", "smtp_from_name": "极光"}

	// Default (no mail_provider): the SMTP account, as before.
	snd, err := buildMailSender(mapGet(smtpVals), mailSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snd.(*mailer.Config); !ok {
		t.Fatalf("default sender %T", snd)
	}

	// Resend selected and fully configured.
	vals := map[string]string{"mail_provider": "resend", "resend_from": "send@resend.example.com", "resend_from_name": "R"}
	snd, err = buildMailSender(mapGet(vals), mailSecrets{resendKey: "re_key"})
	if err != nil {
		t.Fatal(err)
	}
	rc, ok := snd.(*mailer.ResendConfig)
	if !ok {
		t.Fatalf("resend sender %T", snd)
	}
	if rc.APIKey != "re_key" || rc.From != "send@resend.example.com" || rc.FromName != "R" {
		t.Fatalf("resend config %+v", rc)
	}

	// Resend From falls back to the global smtp_from / smtp_from_name.
	vals = map[string]string{"mail_provider": "resend", "smtp_from": "global@example.com", "smtp_from_name": "全局"}
	snd, err = buildMailSender(mapGet(vals), mailSecrets{resendKey: "re_key"})
	if err != nil {
		t.Fatal(err)
	}
	rc = snd.(*mailer.ResendConfig)
	if rc.From != "global@example.com" || rc.FromName != "全局" {
		t.Fatalf("fallback config %+v", rc)
	}

	// Resend selected but key missing: incomplete even though SMTP is
	// fully configured — availability follows the selected channel only.
	vals = map[string]string{"mail_provider": "resend"}
	for k, v := range smtpVals {
		vals[k] = v
	}
	if snd, err := buildMailSender(mapGet(vals), mailSecrets{}); err == nil || snd != nil {
		t.Fatalf("incomplete resend accepted: %v %v", snd, err)
	}

	// Cloudflare selected and fully configured.
	vals = map[string]string{"mail_provider": "cloudflare", "cf_mail_account_id": "acct1", "cf_mail_from": "send@cf.example.com"}
	snd, err = buildMailSender(mapGet(vals), mailSecrets{cfToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	cc, ok := snd.(*mailer.CloudflareConfig)
	if !ok {
		t.Fatalf("cloudflare sender %T", snd)
	}
	if cc.AccountID != "acct1" || cc.APIToken != "tok" || cc.From != "send@cf.example.com" {
		t.Fatalf("cloudflare config %+v", cc)
	}

	// Cloudflare selected but token missing.
	vals = map[string]string{"mail_provider": "cloudflare", "cf_mail_account_id": "acct1", "cf_mail_from": "send@cf.example.com"}
	if snd, err := buildMailSender(mapGet(vals), mailSecrets{}); err == nil || snd != nil {
		t.Fatalf("incomplete cloudflare accepted: %v %v", snd, err)
	}

	// An unknown provider value falls back to SMTP.
	vals = map[string]string{"mail_provider": "pigeon"}
	for k, v := range smtpVals {
		vals[k] = v
	}
	snd, err = buildMailSender(mapGet(vals), mailSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snd.(*mailer.Config); !ok {
		t.Fatalf("unknown provider sender %T", snd)
	}
}

// TestMailOnFollowsSelectedProvider: with only Resend configured and
// selected, mail counts as available (codes, registration, notices);
// switching the selection to the unconfigured SMTP turns it off again.
func TestMailOnFollowsSelectedProvider(t *testing.T) {
	s, _, _ := newAdminClient(t)
	for k, v := range map[string]string{
		"mail_provider": "resend", "resend_api_key": "re_key", "resend_from": "noreply@example.com",
	} {
		s.st.SetSetting(k, v)
	}
	if !s.mailOn() {
		t.Fatal("resend configured and selected, but mail is off")
	}
	if !s.mailLoginOn() {
		t.Fatal("mail code sign-in should follow the selected provider")
	}
	s.st.SetSetting("mail_provider", "smtp")
	if s.mailOn() {
		t.Fatal("smtp selected but not configured, yet mail is on")
	}
	if err := s.sendMail(context.Background(), "user@example.com", "主题", "正文", ""); err == nil ||
		!strings.Contains(err.Error(), "未配置 SMTP") {
		t.Fatalf("sendMail error %v", err)
	}
	s.st.SetSetting("mail_provider", "cloudflare")
	if err := s.sendMail(context.Background(), "user@example.com", "主题", "正文", ""); err == nil ||
		!strings.Contains(err.Error(), "未配置 Cloudflare") {
		t.Fatalf("sendMail error %v", err)
	}
}
