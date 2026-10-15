package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ACME HTTP-01 on behalf of the panel: certificates are issued centrally,
// but the challenge must be answered on port 80 of the host the domain
// points to. The agent serves only /.well-known/acme-challenge/<token>, and
// only while a validation is in progress.

type acmeHTTP struct {
	mu     sync.Mutex
	srv    *http.Server
	port   int
	tokens map[string]string
	timer  *time.Timer
}

const acmeIdle = 10 * time.Minute // safety net if the panel never cleans up

func (h *acmeHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.URL.Path, "/.well-known/acme-challenge/")
	h.mu.Lock()
	ka, found := h.tokens[tok]
	h.mu.Unlock()
	if !ok || !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(ka))
}

// present starts serving a token (and the listener if needed).
func (h *acmeHTTP) present(port int, token, keyAuth string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.srv != nil && h.port != port {
		return fmt.Errorf("a challenge server is already running on port %d", h.port)
	}
	if h.srv == nil {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			if errors.Is(err, syscall.EADDRINUSE) {
				owner := portOwner(portUse{Proto: "tcp", Port: port})
				if owner != "" {
					owner = "（" + owner + "）"
				}
				return fmt.Errorf("端口 %d 已被占用%s，HTTP-01 验证需要它：请临时停止占用的程序，或改用 Cloudflare DNS 验证", port, owner)
			}
			return err
		}
		h.srv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		h.port = port
		h.tokens = map[string]string{}
		go func(s *http.Server) { _ = s.Serve(ln) }(h.srv)
	}
	h.tokens[token] = keyAuth
	if h.timer != nil {
		h.timer.Stop()
	}
	h.timer = time.AfterFunc(acmeIdle, func() { h.stop() })
	return nil
}

// cleanup stops serving a token and closes the listener when none remain.
func (h *acmeHTTP) cleanup(token string) {
	h.mu.Lock()
	delete(h.tokens, token)
	empty := len(h.tokens) == 0
	h.mu.Unlock()
	if empty {
		h.stop()
	}
}

func (h *acmeHTTP) stop() {
	h.mu.Lock()
	srv := h.srv
	h.srv, h.tokens = nil, nil
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// handleACMEHTTP runs the acme_http command: action=present|cleanup.
func (a *Agent) handleACMEHTTP(args map[string]string) error {
	switch args["action"] {
	case "present":
		port, _ := strconv.Atoi(args["port"])
		if port <= 0 || port > 65535 {
			port = 80
		}
		if args["token"] == "" || strings.ContainsAny(args["token"], "/?#") {
			return errors.New("invalid token")
		}
		return a.acme.present(port, args["token"], args["keyauth"])
	case "cleanup":
		a.acme.cleanup(args["token"])
		return nil
	}
	return fmt.Errorf("unknown action %q", args["action"])
}
