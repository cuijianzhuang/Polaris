package panel

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestSourceAddressPolicy(t *testing.T) {
	for _, tc := range []struct {
		addr string
		lan  bool
		ok   bool
	}{
		{"8.8.8.8", false, true}, {"2606:4700:4700::1111", false, true},
		{"127.0.0.1", false, false}, {"::1", false, false}, {"10.0.0.1", false, false},
		{"::ffff:192.168.1.1", false, false}, {"100.64.0.1", false, false},
		{"127.0.0.1", true, true}, {"10.0.0.1", true, true}, {"fd00::1", true, true},
		{"169.254.169.254", true, false}, {"fe80::1", true, false},
		{"0.0.0.0", true, false}, {"::", true, false}, {"0.1.2.3", true, false},
		{"224.0.0.1", true, false}, {"ff02::1", true, false},
	} {
		if err := checkSourceIP(net.ParseIP(tc.addr), tc.lan); (err == nil) != tc.ok {
			t.Errorf("%s lan=%t: %v", tc.addr, tc.lan, err)
		}
	}
}

func TestSourceProxyDestinationPolicy(t *testing.T) {
	proxyURL, _ := url.Parse("http://192.0.2.1:8080")
	proxy := func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	lookup := func(ctx context.Context, network, host string) ([]net.IP, error) {
		switch host {
		case "public.test":
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		case "mixed.test":
			return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("::1")}, nil
		case "empty.test":
			return nil, nil
		default:
			return nil, errors.New("DNS unavailable")
		}
	}
	for _, tc := range []struct {
		host    string
		lan, ok bool
	}{
		{"public.test", false, true}, {"mixed.test", false, false}, {"empty.test", false, false},
		{"missing.test", false, false}, {"10.0.0.1", false, false}, {"[::1]", false, false},
		{"10.0.0.1", true, true}, {"169.254.169.254", true, false},
	} {
		req, _ := http.NewRequest("GET", "https://"+tc.host+"/sub", nil)
		got, err := sourceProxy(tc.lan, proxy, lookup)(req)
		if (err == nil) != tc.ok || tc.ok && got != proxyURL {
			t.Errorf("%s lan=%t: proxy=%v err=%v", tc.host, tc.lan, got, err)
		}
	}
	// NO_PROXY/direct requests use the socket guard, not a preflight DNS result.
	noProxy := func(*http.Request) (*url.URL, error) { return nil, nil }
	req, _ := http.NewRequest("GET", "http://missing.test", nil)
	if p, err := sourceProxy(false, noProxy, lookup)(req); p != nil || err != nil {
		t.Fatalf("direct selection: %v %v", p, err)
	}
}

func TestSourceProxyRedirectBlockedBeforeForwarding(t *testing.T) {
	var calls atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer proxyServer.Close()
	proxyURL, _ := url.Parse(proxyServer.URL)
	s := newTestServer(t)
	client := s.safeClient(time.Second)
	defer client.CloseIdleConnections()
	tr := client.Transport.(*http.Transport)
	tr.Proxy = sourceProxy(false, func(*http.Request) (*url.URL, error) { return proxyURL, nil }, func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil })
	// Route only the trusted test proxy locally; the production socket guard is
	// tested separately. Destination validation must still run on each redirect.
	tr.DialContext = (&net.Dialer{}).DialContext
	resp, err := client.Get("http://public.test/source")
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, errPrivateTarget) || calls.Load() != 1 {
		t.Fatalf("redirect reached proxy: calls=%d err=%v", calls.Load(), err)
	}
}

func TestSourceDirectLANOptIn(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write([]byte("source")) }))
	defer upstream.Close()
	s := newTestServer(t)
	for _, allow := range []bool{false, true} {
		s.st.SetSetting("allow_private_sources", map[bool]string{false: "false", true: "true"}[allow])
		client := s.safeClient(time.Second)
		client.Transport.(*http.Transport).Proxy = nil
		resp, err := client.Get(upstream.URL)
		if allow {
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != "source" {
				t.Fatalf("unexpected body %q", body)
			}
		} else if !errors.Is(err, errPrivateTarget) {
			t.Fatalf("expected refusal: %v", err)
		}
		client.CloseIdleConnections()
	}
	if calls.Load() != 1 {
		t.Fatal("blocked request reached upstream", calls.Load())
	}
}
