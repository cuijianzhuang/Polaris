package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

// The HTTPS providers. BaseURL and Client are overridable so tests can
// point the sender at an httptest server; the zero values hit the real
// APIs with a 15-second client.

const (
	resendDefaultBase     = "https://api.resend.com"
	cloudflareDefaultBase = "https://api.cloudflare.com/client/v4"
)

func apiClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func apiBase(base, def string) string {
	if strings.TrimSpace(base) == "" {
		return def
	}
	return strings.TrimRight(base, "/")
}

// postJSON posts payload with a bearer token and returns the response
// body. A transport error is wrapped with the provider's name.
func postJSON(ctx context.Context, client *http.Client, provider, url, token string, payload any) (int, []byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiClient(client).Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s 发送失败：%w", provider, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("%s 发送失败：%w", provider, err)
	}
	return resp.StatusCode, body, nil
}

// --- Resend ---

// ResendConfig sends through the Resend API (https://resend.com).
type ResendConfig struct {
	APIKey   string
	From     string // address
	FromName string
	BaseURL  string       // tests only
	Client   *http.Client // tests only
}

// Check reports what is missing from the configuration.
func (c *ResendConfig) Check() error {
	switch {
	case strings.TrimSpace(c.APIKey) == "":
		return errors.New("未填写 Resend API 密钥")
	case !Valid(c.From):
		return errors.New("发件地址无效")
	}
	return nil
}

// Send delivers m through Resend.
func (c *ResendConfig) Send(ctx context.Context, m Message) error {
	if err := c.Check(); err != nil {
		return err
	}
	if !Valid(m.To) {
		return fmt.Errorf("收件地址无效：%s", m.To)
	}
	from := c.From
	if c.FromName != "" {
		from = (&mail.Address{Name: c.FromName, Address: c.From}).String()
	}
	payload := map[string]any{
		"from":    from,
		"to":      []string{m.To},
		"subject": m.Subject,
	}
	if m.Text != "" {
		payload["text"] = m.Text
	}
	if m.HTML != "" {
		payload["html"] = m.HTML
	}
	status, body, err := postJSON(ctx, c.Client, "Resend", apiBase(c.BaseURL, resendDefaultBase)+"/emails", c.APIKey, payload)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		var e struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		switch {
		case e.Message != "" && e.Name != "":
			return fmt.Errorf("Resend 发送失败（%s）：%s", e.Name, e.Message)
		case e.Message != "":
			return fmt.Errorf("Resend 发送失败：%s", e.Message)
		}
		return fmt.Errorf("Resend 发送失败：HTTP %d", status)
	}
	return nil
}

// --- Cloudflare Email Sending ---

// CloudflareConfig sends through the Cloudflare Email Sending API; the
// API token needs the Email Sending permission and the From domain must
// be onboarded for Email Sending on the account.
type CloudflareConfig struct {
	AccountID string
	APIToken  string
	From      string // address
	FromName  string
	BaseURL   string       // tests only
	Client    *http.Client // tests only
}

// Check reports what is missing from the configuration.
func (c *CloudflareConfig) Check() error {
	switch {
	case strings.TrimSpace(c.AccountID) == "":
		return errors.New("未填写 Cloudflare 账户 ID")
	case strings.TrimSpace(c.APIToken) == "":
		return errors.New("未填写 Cloudflare API 令牌")
	case !Valid(c.From):
		return errors.New("发件地址无效")
	}
	return nil
}

// Send delivers m through Cloudflare Email Sending.
func (c *CloudflareConfig) Send(ctx context.Context, m Message) error {
	if err := c.Check(); err != nil {
		return err
	}
	if !Valid(m.To) {
		return fmt.Errorf("收件地址无效：%s", m.To)
	}
	var from any = c.From
	if c.FromName != "" {
		from = map[string]string{"address": c.From, "name": c.FromName}
	}
	payload := map[string]any{
		"from":    from,
		"to":      m.To,
		"subject": m.Subject,
	}
	if m.Text != "" {
		payload["text"] = m.Text
	}
	if m.HTML != "" {
		payload["html"] = m.HTML
	}
	url := apiBase(c.BaseURL, cloudflareDefaultBase) + "/accounts/" + c.AccountID + "/email/sending/send"
	status, body, err := postJSON(ctx, c.Client, "Cloudflare", url, c.APIToken, payload)
	if err != nil {
		return err
	}
	var r struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(body, &r)
	if status < 200 || status >= 300 || !r.Success {
		if len(r.Errors) > 0 && r.Errors[0].Message != "" {
			return fmt.Errorf("Cloudflare 发送失败：%s", r.Errors[0].Message)
		}
		return fmt.Errorf("Cloudflare 发送失败：HTTP %d", status)
	}
	return nil
}
