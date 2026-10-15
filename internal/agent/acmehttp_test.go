package agent

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestACMEHTTPServer(t *testing.T) {
	port := freePortT(t)
	var h acmeHTTP
	if err := h.present(port, "tok1", "tok1.key"); err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, string) {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/.well-known/acme-challenge/tok1"); code != 200 || body != "tok1.key" {
		t.Fatalf("challenge: %d %q", code, body)
	}
	if code, _ := get("/.well-known/acme-challenge/other"); code != 404 {
		t.Fatalf("unknown token: %d", code)
	}
	if code, _ := get("/etc/passwd"); code != 404 {
		t.Fatalf("other paths: %d", code)
	}
	h.cleanup("tok1")
	if code, _ := get("/.well-known/acme-challenge/tok1"); code != 0 {
		t.Fatal("listener still up after cleanup")
	}
	// A busy port is reported plainly.
	l, _ := net.Listen("tcp", ":"+strconv.Itoa(port))
	defer l.Close()
	if err := h.present(port, "t", "k"); err == nil || !strings.Contains(err.Error(), "已被占用") {
		t.Fatalf("busy port: %v", err)
	}
}

func freePortT(t *testing.T) int {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
