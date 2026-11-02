package model

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestRemoteTunnelTokenValidation(t *testing.T) {
	id := "12345678-abcd-1234-abcd-123456789abc"
	makeToken := func(account, uuid string, secret []byte) string {
		b, _ := json.Marshal(map[string]any{"a": account, "t": uuid, "s": secret, "e": "optional-endpoint"})
		return base64.StdEncoding.EncodeToString(b)
	}
	token := makeToken(strings.Repeat("a", 32), id, []byte(strings.Repeat("s", 32)))
	got, err := NormalizeRemoteTunnelToken([]byte("\n"+token+"\n"), id)
	if err != nil || string(got) != token {
		t.Fatal("opaque token was not retained", err)
	}
	for _, raw := range []string{"", "api-token", "cloudflared service install " + token, token[:10] + "\n" + token[10:], makeToken("invalid", id, []byte(strings.Repeat("s", 32))), makeToken(strings.Repeat("a", 32), "22345678-abcd-1234-abcd-123456789abc", []byte(strings.Repeat("s", 32))), makeToken(strings.Repeat("a", 32), id, []byte("short")), strings.Repeat("x", 16385)} {
		if _, err := NormalizeRemoteTunnelToken([]byte(raw), id); err == nil || strings.Contains(err.Error(), token) {
			t.Fatal("invalid token accepted or leaked")
		}
	}
	if _, err := NormalizeManagedTunnelCredential([]byte(token), id, "local"); err == nil {
		t.Fatal("remote token accepted as local JSON")
	}
	spec := TunnelSpec{ProfileID: "test", ID: id, CredentialVersion: strings.Repeat("a", 16), CredentialMode: "remote"}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	spec.Routes = []TunnelRoute{{Service: "http_status:404"}}
	if err := spec.Validate(); err == nil {
		t.Fatal("local routes accepted for remote tunnel")
	}
	spec.Routes = nil
	spec.CredentialMode = "unknown"
	if err := spec.Validate(); err == nil {
		t.Fatal("unknown mode accepted")
	}
}
