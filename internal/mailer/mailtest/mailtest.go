// Package mailtest is an in-memory SMTP server for tests: plain
// connections, no auth, every message kept.
package mailtest

import (
	"bufio"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"sync"
)

// Mail is one received message.
type Mail struct {
	To      string
	Subject string
	Body    string // decoded (quoted-printable)
}

// Server receives mail on 127.0.0.1.
type Server struct {
	ln   net.Listener
	mu   sync.Mutex
	mail []Mail
}

// Start listens until Close.
func Start() (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s, nil
}

// Port is the listening port.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Close stops the server.
func (s *Server) Close() { s.ln.Close() }

// Mails returns what was received so far.
func (s *Server) Mails() []Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Mail(nil), s.mail...)
}

// To returns the mail sent to addr.
func (s *Server) To(addr string) []Mail {
	var out []Mail
	for _, m := range s.Mails() {
		if m.To == addr {
			out = append(out, m)
		}
	}
	return out
}

func (s *Server) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(l string) { io.WriteString(c, l+"\r\n") }
	say("220 mailtest")
	var to string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0]); cmd {
		case "EHLO", "HELO":
			say("250 mailtest")
		case "RCPT":
			to = strings.Trim(line[strings.Index(line, ":")+1:], " <>")
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
				sb.WriteString(strings.TrimPrefix(l, "."))
			}
			m := Mail{To: to}
			if msg, err := mail.ReadMessage(strings.NewReader(sb.String())); err == nil {
				m.Subject, _ = new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
				b, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
				m.Body = string(b)
			}
			s.mu.Lock()
			s.mail = append(s.mail, m)
			s.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}
