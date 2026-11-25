package mailer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capture records the last request an API test server received.
type capture struct {
	path, auth, contentType string
	body                    map[string]any
}

func apiServer(t *testing.T, cap *capture, status int, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.auth = r.Header.Get("Authorization")
		cap.contentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		cap.body = nil
		if err := json.Unmarshal(raw, &cap.body); err != nil {
			t.Errorf("request body is not JSON: %v (%s)", err, raw)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
}

func TestResendSend(t *testing.T) {
	var cap capture
	srv := apiServer(t, &cap, http.StatusOK, `{"id":"em_123"}`)
	defer srv.Close()
	c := &ResendConfig{APIKey: "re_test_key", From: "noreply@example.com", FromName: "极光", BaseURL: srv.URL}
	msg := Message{To: "user@example.com", Subject: "验证码", Text: "正文", HTML: "<p>正文</p>"}
	if err := c.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if cap.path != "/emails" {
		t.Fatalf("path %q", cap.path)
	}
	if cap.auth != "Bearer re_test_key" {
		t.Fatalf("auth %q", cap.auth)
	}
	if cap.contentType != "application/json" {
		t.Fatalf("content type %q", cap.contentType)
	}
	if got := cap.body["from"]; got != "=?utf-8?q?=E6=9E=81=E5=85=89?= <noreply@example.com>" && got != "极光 <noreply@example.com>" {
		t.Fatalf("from %v", got)
	}
	if to, ok := cap.body["to"].([]any); !ok || len(to) != 1 || to[0] != "user@example.com" {
		t.Fatalf("to %v", cap.body["to"])
	}
	if cap.body["subject"] != "验证码" || cap.body["text"] != "正文" || cap.body["html"] != "<p>正文</p>" {
		t.Fatalf("body %v", cap.body)
	}
}

func TestResendSendOmitsEmptyPartsAndName(t *testing.T) {
	var cap capture
	srv := apiServer(t, &cap, http.StatusOK, `{"id":"em_1"}`)
	defer srv.Close()
	c := &ResendConfig{APIKey: "k", From: "a@example.com", BaseURL: srv.URL}
	if err := c.Send(context.Background(), Message{To: "b@example.com", Subject: "s", Text: "t"}); err != nil {
		t.Fatal(err)
	}
	if cap.body["from"] != "a@example.com" {
		t.Fatalf("from %v", cap.body["from"])
	}
	if _, ok := cap.body["html"]; ok {
		t.Fatalf("html should be omitted: %v", cap.body)
	}
}

func TestResendErrorMapping(t *testing.T) {
	var cap capture
	srv := apiServer(t, &cap, http.StatusForbidden, `{"statusCode":403,"name":"validation_error","message":"domain not verified"}`)
	defer srv.Close()
	c := &ResendConfig{APIKey: "k", From: "a@example.com", BaseURL: srv.URL}
	err := c.Send(context.Background(), Message{To: "b@example.com", Subject: "s", Text: "t"})
	if err == nil || !strings.Contains(err.Error(), "validation_error") || !strings.Contains(err.Error(), "domain not verified") {
		t.Fatalf("err %v", err)
	}
}

func TestResendCheck(t *testing.T) {
	if err := (&ResendConfig{From: "a@example.com"}).Check(); err == nil {
		t.Fatal("missing key accepted")
	}
	if err := (&ResendConfig{APIKey: "k", From: "nope"}).Check(); err == nil {
		t.Fatal("bad from accepted")
	}
	if err := (&ResendConfig{APIKey: "k", From: "a@example.com"}).Check(); err != nil {
		t.Fatal(err)
	}
}

func TestCloudflareSend(t *testing.T) {
	var cap capture
	srv := apiServer(t, &cap, http.StatusOK, `{"success":true,"errors":[],"messages":[],"result":{"message_id":"m1"}}`)
	defer srv.Close()
	c := &CloudflareConfig{AccountID: "acct42", APIToken: "cf_token", From: "noreply@example.com", FromName: "极光", BaseURL: srv.URL}
	msg := Message{To: "user@example.com", Subject: "验证码", Text: "正文", HTML: "<p>正文</p>"}
	if err := c.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if cap.path != "/accounts/acct42/email/sending/send" {
		t.Fatalf("path %q", cap.path)
	}
	if cap.auth != "Bearer cf_token" {
		t.Fatalf("auth %q", cap.auth)
	}
	from, ok := cap.body["from"].(map[string]any)
	if !ok || from["address"] != "noreply@example.com" || from["name"] != "极光" {
		t.Fatalf("from %v", cap.body["from"])
	}
	if cap.body["to"] != "user@example.com" || cap.body["subject"] != "验证码" || cap.body["text"] != "正文" || cap.body["html"] != "<p>正文</p>" {
		t.Fatalf("body %v", cap.body)
	}
}

func TestCloudflareSendBareFromAndOmissions(t *testing.T) {
	var cap capture
	srv := apiServer(t, &cap, http.StatusOK, `{"success":true,"result":{}}`)
	defer srv.Close()
	c := &CloudflareConfig{AccountID: "a", APIToken: "t", From: "a@example.com", BaseURL: srv.URL}
	if err := c.Send(context.Background(), Message{To: "b@example.com", Subject: "s", HTML: "<b>x</b>"}); err != nil {
		t.Fatal(err)
	}
	if cap.body["from"] != "a@example.com" {
		t.Fatalf("from %v", cap.body["from"])
	}
	if _, ok := cap.body["text"]; ok {
		t.Fatalf("text should be omitted: %v", cap.body)
	}
}

func TestCloudflareErrorMapping(t *testing.T) {
	var cap capture
	srv := apiServer(t, &cap, http.StatusBadRequest, `{"success":false,"errors":[{"code":10001,"message":"sender domain not onboarded"}],"result":null}`)
	defer srv.Close()
	c := &CloudflareConfig{AccountID: "a", APIToken: "t", From: "a@example.com", BaseURL: srv.URL}
	err := c.Send(context.Background(), Message{To: "b@example.com", Subject: "s", Text: "t"})
	if err == nil || !strings.Contains(err.Error(), "sender domain not onboarded") {
		t.Fatalf("err %v", err)
	}
	// A 200 with success=false is still a failure.
	srv2 := apiServer(t, &cap, http.StatusOK, `{"success":false,"errors":[{"message":"quota exceeded"}]}`)
	defer srv2.Close()
	c.BaseURL = srv2.URL
	err = c.Send(context.Background(), Message{To: "b@example.com", Subject: "s", Text: "t"})
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("err %v", err)
	}
}

func TestCloudflareCheck(t *testing.T) {
	if err := (&CloudflareConfig{APIToken: "t", From: "a@example.com"}).Check(); err == nil {
		t.Fatal("missing account accepted")
	}
	if err := (&CloudflareConfig{AccountID: "a", From: "a@example.com"}).Check(); err == nil {
		t.Fatal("missing token accepted")
	}
	if err := (&CloudflareConfig{AccountID: "a", APIToken: "t", From: "a@example.com"}).Check(); err != nil {
		t.Fatal(err)
	}
}
