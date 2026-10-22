package warp

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// FakeAPI imitates the WARP registration API. It records requests.
type FakeAPI struct {
	mu       sync.Mutex
	Calls    []string
	Key      string // public key the client sent
	Licenses []string
	Reject   bool
}

func (f *FakeAPI) Serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.Calls = append(f.Calls, r.Method+" "+r.URL.Path)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/reg":
			if f.Reject {
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"errors":[{"message":"Too many registrations"}]}`))
				return
			}
			f.Key, _ = body["key"].(string)
			w.Write([]byte(`{"id":"dev-1","token":"tok-1","account":{"account_type":"free","warp_plus":false},
				"config":{"client_id":"AQID","peers":[{"public_key":"bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=",
				"endpoint":{"v4":"162.159.192.1:0","v6":"[2606:4700:d0::a29f:c001]:0","host":"engage.cloudflareclient.com:2408"}}],
				"interface":{"addresses":{"v4":"172.16.0.2","v6":"2606:4700:110:8a36::1"}}}}`))
		case r.Header.Get("Authorization") != "Bearer tok-1":
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodPatch && r.URL.Path == "/reg/dev-1":
			w.Write([]byte(`{}`))
		case r.Method == http.MethodPut && r.URL.Path == "/reg/dev-1/account":
			l, _ := body["license"].(string)
			f.Licenses = append(f.Licenses, l)
			if l != "good-license" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"errors":[{"message":"Invalid license"}]}`))
				return
			}
			w.Write([]byte(`{"account_type":"unlimited","warp_plus":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	old := API
	API = srv.URL
	t.Cleanup(func() { API = old; srv.Close() })
}

func TestRegister(t *testing.T) {
	f := &FakeAPI{}
	f.Serve(t)
	d, err := Register(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Endpoint != "162.159.192.1" || d.PeerPublicKey == "" || strings.Join(d.LocalAddresses(), ",") != "172.16.0.2/32,2606:4700:110:8a36::1/128" {
		t.Fatalf("device %+v", d)
	}
	if len(d.Reserved) != 3 || d.Reserved[0] != 1 || d.Reserved[1] != 2 || d.Reserved[2] != 3 {
		t.Fatalf("reserved %v", d.Reserved)
	}
	// The key pair is ours: the API saw only the public half, and it matches.
	raw, _ := base64.StdEncoding.DecodeString(d.PrivateKey)
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil || base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()) != f.Key || f.Key != d.PublicKey {
		t.Fatalf("key pair mismatch: %v", err)
	}
	if strings.Join(f.Calls, ",") != "POST /reg,PATCH /reg/dev-1" {
		t.Fatalf("calls %v", f.Calls)
	}
}

func TestRegisterLicense(t *testing.T) {
	f := &FakeAPI{}
	f.Serve(t)
	d, err := Register(t.Context(), " good-license ")
	if err != nil || !d.WarpPlus || d.AccountType != "unlimited" {
		t.Fatalf("warp+: %+v %v", d, err)
	}
	if _, err := Register(t.Context(), "bad"); err == nil || !strings.Contains(err.Error(), "Invalid license") {
		t.Fatalf("bad license: %v", err)
	}
	f.Reject = true
	if _, err := Register(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "Too many") {
		t.Fatalf("rejected: %v", err)
	}
}
