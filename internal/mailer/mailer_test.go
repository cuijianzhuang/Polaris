package mailer

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

func testCert(t *testing.T) tls.Certificate {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

// fakeSMTP is a minimal SMTP server: EHLO, STARTTLS, AUTH PLAIN/LOGIN,
// MAIL, RCPT, DATA, QUIT.
type fakeSMTP struct {
	ln       net.Listener
	cert     tls.Certificate
	implicit bool // TLS from the first byte
	login    bool // offer only AUTH LOGIN
	mu       sync.Mutex
	authed   string
	from, to string
	data     string
	sawTLS   bool
}

func newFake(t *testing.T, implicit, login bool) *fakeSMTP {
	f := &fakeSMTP{cert: testCert(t), implicit: implicit, login: login}
	var err error
	if implicit {
		f.ln, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{f.cert}})
	} else {
		f.ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.ln.Close() })
	go func() {
		for {
			c, err := f.ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	tlsOn := f.implicit
	r, w := bufio.NewReader(c), c
	say := func(s string) { io.WriteString(w, s+"\r\n") }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch cmd {
		case "EHLO", "HELO":
			auth := "AUTH PLAIN LOGIN"
			if f.login {
				auth = "AUTH LOGIN"
			}
			if tlsOn {
				say("250-fake\r\n250 " + auth)
			} else {
				say("250-fake\r\n250-STARTTLS\r\n250 " + auth)
			}
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			if tc.Handshake() != nil {
				return
			}
			c, tlsOn = tc, true
			r, w = bufio.NewReader(tc), tc
			f.mu.Lock()
			f.sawTLS = true
			f.mu.Unlock()
		case "AUTH":
			parts := strings.Fields(line)
			var user, pass string
			if strings.EqualFold(parts[1], "PLAIN") {
				raw := ""
				if len(parts) > 2 {
					raw = parts[2]
				} else {
					say("334 ")
					raw, _ = r.ReadString('\n')
				}
				b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
				p := strings.Split(string(b), "\x00")
				user, pass = p[1], p[2]
			} else {
				say("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
				u, _ := r.ReadString('\n')
				say("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
				pw, _ := r.ReadString('\n')
				ub, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(u))
				pb, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(pw))
				user, pass = string(ub), string(pb)
			}
			if pass != "secret" {
				say("535 bad credentials")
				continue
			}
			f.mu.Lock()
			f.authed = user
			f.mu.Unlock()
			say("235 ok")
		case "MAIL":
			f.mu.Lock()
			f.from = line
			f.mu.Unlock()
			say("250 ok")
		case "RCPT":
			f.mu.Lock()
			f.to = line
			f.mu.Unlock()
			say("250 ok")
		case "DATA":
			say("354 go")
			var sb strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				sb.WriteString(l)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func TestSend(t *testing.T) {
	for _, tc := range []struct {
		name            string
		security        string
		implicit, login bool
	}{
		{"starttls", StartTLS, false, false},
		{"tls", TLS, true, false},
		{"login-only", StartTLS, false, true},
		{"none", None, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, tc.implicit, tc.login)
			c := &Config{Host: "127.0.0.1", Port: f.port(), Security: tc.security, User: "bot@example.com", Pass: "secret",
				From: "bot@example.com", FromName: "极光 Polaris", InsecureSkipVerify: true}
			err := c.Send(context.Background(), Message{To: "user@example.org", Subject: "流量提醒：已用 80%", Text: "你好\n已用 80%", HTML: "<p>你好</p>"})
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.authed != "bot@example.com" || !strings.Contains(f.from, "<bot@example.com>") || !strings.Contains(f.to, "<user@example.org>") {
				t.Fatalf("envelope: auth %q from %q to %q", f.authed, f.from, f.to)
			}
			if tc.security == StartTLS && !f.sawTLS {
				t.Fatal("no STARTTLS")
			}
			msg, err := mail.ReadMessage(strings.NewReader(f.data))
			if err != nil {
				t.Fatal(err)
			}
			subj, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
			from, _ := mail.ParseAddress(msg.Header.Get("From"))
			if subj != "流量提醒：已用 80%" || from.Name != "极光 Polaris" || !strings.Contains(msg.Header.Get("Content-Type"), "multipart/alternative") {
				t.Fatalf("headers: %q %q %q", subj, from.Name, msg.Header.Get("Content-Type"))
			}
			body, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(f.data)))
			if !strings.Contains(string(body), "你好") || !strings.Contains(string(body), "<p>你好</p>") {
				t.Fatalf("body %s", body)
			}
		})
	}
}

func TestSendErrors(t *testing.T) {
	f := newFake(t, false, false)
	base := Config{Host: "127.0.0.1", Port: f.port(), User: "bot@example.com", Pass: "wrong", From: "bot@example.com", InsecureSkipVerify: true}
	if err := base.Send(context.Background(), Message{To: "u@example.org", Subject: "x", Text: "x"}); err == nil || !strings.Contains(err.Error(), "登录失败") {
		t.Fatalf("bad password: %v", err)
	}
	c := base
	c.InsecureSkipVerify = false
	c.Pass = "secret"
	if err := c.Send(context.Background(), Message{To: "u@example.org", Subject: "x", Text: "x"}); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("untrusted cert: %v", err)
	}
	if err := base.Send(context.Background(), Message{To: "not-an-address", Subject: "x"}); err == nil {
		t.Fatal("bad recipient accepted")
	}
	if err := (&Config{From: "a@b.c"}).Check(); err == nil {
		t.Fatal("missing host accepted")
	}
}

func TestValid(t *testing.T) {
	for addr, ok := range map[string]bool{"a@b.co": true, "A.B+c@example.com": true, "a@b": false, "a b@c.d": false, "<a@b.c>": false, "x": false, "a@b.c\r\nBcc: x": false} {
		if Valid(addr) != ok {
			t.Errorf("Valid(%q) != %v", addr, ok)
		}
	}
	if Normalize(" User@Example.COM ") != "user@example.com" || Normalize("bad") != "" {
		t.Fatal("normalize")
	}
}
