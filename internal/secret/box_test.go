package secret

import "testing"

func TestRoundTrip(t *testing.T) {
	b, _ := New("k1")
	s := b.Seal("private-key")
	if s == "private-key" || b.MustOpen(s) != "private-key" {
		t.Fatal("roundtrip failed")
	}
	if b.Seal(s) != s {
		t.Fatal("double seal must be a no-op")
	}
	if v, _ := b.Open("plain"); v != "plain" {
		t.Fatal("unsealed values pass through")
	}
	other, _ := New("k2")
	if _, err := other.Open(s); err == nil {
		t.Fatal("wrong key must fail")
	}
}
