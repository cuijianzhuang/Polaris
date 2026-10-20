// Package mailer sends mail through an SMTP server: STARTTLS (port 587),
// implicit TLS (465) or plain (25, local relays), with PLAIN/LOGIN auth and
// UTF-8 text + HTML bodies.
package mailer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Security is how the connection is protected.
const (
	StartTLS = "starttls"
	TLS      = "tls"
	None     = "none"
)

// Config is an SMTP account.
type Config struct {
	Host     string
	Port     int
	Security string // starttls (default) | tls | none
	User     string
	Pass     string
	From     string // address
	FromName string
	// InsecureSkipVerify is for tests only.
	InsecureSkipVerify bool
}

// Message is one mail.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string // optional
}

// Valid reports whether addr is a plain e-mail address.
func Valid(addr string) bool {
	if len(addr) > 254 || strings.ContainsAny(addr, " <>\r\n") {
		return false
	}
	a, err := mail.ParseAddress(addr)
	return err == nil && a.Address == addr && strings.Contains(addr[strings.LastIndex(addr, "@")+1:], ".")
}

// Normalize lower-cases and trims an address ("" when invalid).
func Normalize(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if !Valid(addr) {
		return ""
	}
	return addr
}

func (c *Config) port() int {
	if c.Port > 0 {
		return c.Port
	}
	switch c.Security {
	case TLS:
		return 465
	case None:
		return 25
	}
	return 587
}

// Check reports what is missing from the account.
func (c *Config) Check() error {
	switch {
	case c.Host == "":
		return errors.New("未填写 SMTP 服务器")
	case !Valid(c.From):
		return errors.New("发件地址无效")
	case c.Security != "" && c.Security != StartTLS && c.Security != TLS && c.Security != None:
		return errors.New("加密方式应为 starttls、tls 或 none")
	}
	return nil
}

// Send delivers m.
func (c *Config) Send(ctx context.Context, m Message) error {
	if err := c.Check(); err != nil {
		return err
	}
	if !Valid(m.To) {
		return fmt.Errorf("收件地址无效：%s", m.To)
	}
	raw, err := c.build(m)
	if err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.port()))
	tlsConf := &tls.Config{ServerName: c.Host, InsecureSkipVerify: c.InsecureSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // tests only
	d := &net.Dialer{Deadline: deadline}
	var conn net.Conn
	if c.Security == TLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, tlsConf)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败：%w", err)
	}
	_ = conn.SetDeadline(deadline)
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SMTP 握手失败：%w", err)
	}
	defer cl.Close()
	if err := cl.Hello(helloName()); err != nil {
		return err
	}
	if c.Security == "" || c.Security == StartTLS {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("服务器不支持 STARTTLS：请改用 TLS（465 端口）或不加密")
		}
		if err := cl.StartTLS(tlsConf); err != nil {
			return fmt.Errorf("STARTTLS 失败：%w", err)
		}
	}
	if c.User != "" {
		if err := cl.Auth(c.auth()); err != nil {
			return fmt.Errorf("SMTP 登录失败：%w", err)
		}
	}
	if err := cl.Mail(c.From); err != nil {
		return fmt.Errorf("发件地址被拒绝：%w", err)
	}
	if err := cl.Rcpt(m.To); err != nil {
		return fmt.Errorf("收件地址被拒绝：%w", err)
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("发送失败：%w", err)
	}
	return cl.Quit()
}

func helloName() string {
	return "localhost"
}

// auth picks PLAIN, or LOGIN when the server only offers that. Both need
// TLS except to localhost; plain connections to a relay are allowed with
// LOGIN-free relays (User empty).
func (c *Config) auth() smtp.Auth {
	return &plainOrLogin{user: c.User, pass: c.Pass, host: c.Host, insecure: c.Security == None}
}

type plainOrLogin struct {
	user, pass, host string
	insecure         bool
	login            bool
}

func (a *plainOrLogin) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !a.insecure {
		return "", nil, errors.New("拒绝在未加密的连接上发送密码")
	}
	plain := false
	for _, m := range server.Auth {
		if strings.EqualFold(m, "PLAIN") {
			plain = true
		}
	}
	if !plain {
		for _, m := range server.Auth {
			if strings.EqualFold(m, "LOGIN") {
				a.login = true
				return "LOGIN", nil, nil
			}
		}
	}
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a *plainOrLogin) Next(from []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	if !a.login {
		return nil, errors.New("unexpected server challenge")
	}
	switch strings.ToLower(strings.TrimSpace(string(from))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("unexpected LOGIN challenge %q", from)
}

// build writes the message with headers.
func (c *Config) build(m Message) ([]byte, error) {
	var b bytes.Buffer
	from := c.From
	if c.FromName != "" {
		from = (&mail.Address{Name: c.FromName, Address: c.From}).String()
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := c.From[strings.LastIndex(c.From, "@")+1:]
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	hdr("From", from)
	hdr("To", m.To)
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	hdr("MIME-Version", "1.0")
	part := func(ctype, body string) error {
		fmt.Fprintf(&b, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", ctype)
		w := quotedprintable.NewWriter(&b)
		if _, err := w.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n"))); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		b.WriteString("\r\n")
		return nil
	}
	if m.HTML == "" {
		return b.Bytes(), part("text/plain", m.Text)
	}
	boundary := "polaris-" + hex.EncodeToString(id)
	hdr("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	for _, p := range [][2]string{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		b.WriteString("--" + boundary + "\r\n")
		if err := part(p[0], p[1]); err != nil {
			return nil, err
		}
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}
