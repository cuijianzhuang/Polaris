package routedb

import (
	"errors"
	"testing"
)

var errInvalid = errors.New("invalid policy")

func TestDecodePolicy(t *testing.T) {
	p, err := DecodePolicy("", errInvalid)
	if err != nil || p != (Policy{}) {
		t.Fatalf("empty: %+v %v", p, err)
	}
	p, err = DecodePolicy(`{"enabled":true,"sha256":"`+hash64+`","revision":3}`, errInvalid)
	if err != nil || !p.Enabled || p.SHA256 != hash64 || p.Revision != 3 {
		t.Fatalf("valid: %+v %v", p, err)
	}
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"malformed", "{nope"},
		{"negative revision", `{"revision":-1}`},
		{"bad hash", `{"sha256":"xyz"}`},
		{"uppercase hash", `{"sha256":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`},
		{"enabled without hash", `{"enabled":true}`},
	} {
		if _, err := DecodePolicy(tc.value, errInvalid); !errors.Is(err, errInvalid) {
			t.Errorf("%s: err %v, want %v", tc.name, err, errInvalid)
		}
	}
	// Disabled with no snapshot is fine; a hash without enabled is fine.
	if _, err := DecodePolicy(`{"sha256":"`+hash64+`"}`, errInvalid); err != nil {
		t.Fatalf("hash only: %v", err)
	}
}

const hash64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestHash(t *testing.T) {
	if !Hash.MatchString(hash64) || Hash.MatchString("0123") || Hash.MatchString(hash64+"00") {
		t.Fatal("Hash")
	}
}
