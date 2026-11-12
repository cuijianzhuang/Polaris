package secret

import (
	"strings"
	"testing"
)

func TestKidFor(t *testing.T) {
	if KidFor("same-passphrase") != KidFor("same-passphrase") {
		t.Fatal("kid derivation is not deterministic")
	}
	if KidFor("a") == KidFor("b") {
		t.Fatal("different passphrases must give different kids")
	}
	kid := KidFor("anything")
	if len(kid) != 8 {
		t.Fatalf("kid %q is not 8 hex chars", kid)
	}
	for _, c := range kid {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("kid %q is not hex", kid)
		}
	}
}

func TestBoxSetSealAndOpen(t *testing.T) {
	bs, err := NewBoxSet("new-key-0123456789", "old-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	sealed := bs.Seal("hello")
	if !strings.HasPrefix(sealed, "enc:v2:"+bs.CurrentKid()+":") {
		t.Fatalf("seal did not write v2 with the current kid: %q", sealed)
	}
	if got, err := bs.Open(sealed); err != nil || got != "hello" {
		t.Fatalf("open sealed: %q %v", got, err)
	}
	// Sealing plaintext encrypts it with the current key; opening
	// unsealed input returns it unchanged.
	if sealed := bs.Seal("plain"); !strings.HasPrefix(sealed, "enc:v2:"+bs.CurrentKid()+":") {
		t.Fatalf("seal plaintext: %q", sealed)
	} else if got, err := bs.Open(sealed); err != nil || got != "plain" {
		t.Fatalf("open sealed plaintext: %q %v", got, err)
	}
	if got, err := bs.Open("plain"); err != nil || got != "plain" {
		t.Fatalf("open plaintext: %q %v", got, err)
	}
	if bs.Seal("") != "" {
		t.Fatal("seal changed an empty string")
	}
	if bs.MustOpen("nonsense") != "nonsense" {
		t.Fatal("MustOpen changed plaintext")
	}
}

func TestBoxSetOpensV1WithBothKeys(t *testing.T) {
	oldBox, _ := New("old-key-0123456789")
	newBox, _ := New("new-key-0123456789")
	oldV1 := oldBox.Seal("from-old")
	newV1 := newBox.Seal("from-new")

	bs, err := NewBoxSet("new-key-0123456789", "old-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := bs.Open(oldV1); err != nil || got != "from-old" {
		t.Fatalf("v1 sealed with previous key: %q %v", got, err)
	}
	if got, err := bs.Open(newV1); err != nil || got != "from-new" {
		t.Fatalf("v1 sealed with current key: %q %v", got, err)
	}
	// Sealing an old v1 value upgrades it to v2 with the current kid.
	if up := bs.Seal(oldV1); !strings.HasPrefix(up, "enc:v2:"+bs.CurrentKid()+":") {
		t.Fatalf("seal did not upgrade v1: %q", up)
	} else if got, err := bs.Open(up); err != nil || got != "from-old" {
		t.Fatalf("upgraded value: %q %v", got, err)
	}
	// A v1 value sealed with a key the set does not know survives Seal
	// unchanged instead of being destroyed.
	otherBox, _ := New("third-key-0123456789")
	foreign := otherBox.Seal("foreign")
	if got := bs.Seal(foreign); got != foreign {
		t.Fatalf("seal rewrote an unopenable value: %q", got)
	}
	if _, err := bs.Open(foreign); err == nil {
		t.Fatal("foreign v1 value opened without its key")
	}
	if bs.MustOpen(foreign) != "" {
		t.Fatal("MustOpen returned data for an unopenable value")
	}
}

func TestBoxSetRejectsUnknownKid(t *testing.T) {
	bs, _ := NewBoxSet("new-key-0123456789", "old-key-0123456789")
	other, _ := NewBoxSet("third-key-0123456789", "")
	foreignV2 := other.Seal("foreign")
	if _, err := bs.Open(foreignV2); err == nil || !strings.Contains(err.Error(), "unknown key id") {
		t.Fatalf("unknown kid accepted: %v", err)
	}
	if bs.MustOpen(foreignV2) != "" {
		t.Fatal("MustOpen returned data for an unknown kid")
	}
	// Seal must not rewrite it either.
	if got := bs.Seal(foreignV2); got != foreignV2 {
		t.Fatalf("seal rewrote an unknown-kid value: %q", got)
	}
	// Malformed v2 is an error, never plaintext.
	if _, err := bs.Open("enc:v2:zzzz:not-base64"); err == nil {
		t.Fatal("malformed v2 accepted")
	}
	if _, err := bs.Open("enc:v2:abcd:@@@"); err == nil {
		t.Fatal("malformed v2 payload accepted")
	}
}

func TestBoxSetReseal(t *testing.T) {
	oldBox, _ := New("old-key-0123456789")
	bs, _ := NewBoxSet("new-key-0123456789", "old-key-0123456789")

	// Plaintext is left alone.
	if got, err := bs.Reseal("plain"); err != nil || got != "plain" {
		t.Fatalf("reseal plaintext: %q %v", got, err)
	}
	// v1 is upgraded.
	if got, err := bs.Reseal(oldBox.Seal("x")); err != nil || !strings.HasPrefix(got, "enc:v2:"+bs.CurrentKid()+":") {
		t.Fatalf("reseal v1: %q %v", got, err)
	}
	// v2 with the current kid is a no-op.
	cur := bs.Seal("x")
	if got, err := bs.Reseal(cur); err != nil || got != cur {
		t.Fatalf("reseal current v2 changed it: %q %v", got, err)
	}
	// v2 with the previous kid is upgraded.
	prev, _ := NewBoxSet("old-key-0123456789", "")
	prevV2 := prev.Seal("y")
	if got, err := bs.Reseal(prevV2); err != nil || !strings.HasPrefix(got, "enc:v2:"+bs.CurrentKid()+":") {
		t.Fatalf("reseal previous v2: %q %v", got, err)
	} else if pt, err := bs.Open(got); err != nil || pt != "y" {
		t.Fatalf("resealed previous v2: %q %v", pt, err)
	}
	// Unopenable is an error.
	otherBox, _ := New("third-key-0123456789")
	if _, err := bs.Reseal(otherBox.Seal("z")); err == nil {
		t.Fatal("reseal accepted an unopenable value")
	}
}

func TestBoxSetResealText(t *testing.T) {
	oldBox, _ := New("old-key-0123456789")
	bs, _ := NewBoxSet("new-key-0123456789", "old-key-0123456789")
	doc := `{"key": "` + oldBox.Seal("a") + `", "other": "` + bs.Seal("b") + `", "plain": "c"}`
	out, changed, failed := bs.ResealText(doc)
	if changed != 1 || failed != 0 {
		t.Fatalf("resealText: changed=%d failed=%d", changed, failed)
	}
	if strings.Contains(out, "enc:v1:") {
		t.Fatal("v1 token survived resealText")
	}
	if pt, err := bs.Open(strings.Split(strings.Split(out, `"key": "`)[1], `"`)[0]); err != nil || pt != "a" {
		t.Fatalf("resealed token: %q %v", pt, err)
	}
	// A token that cannot be opened is left in place and counted.
	otherBox, _ := New("third-key-0123456789")
	bad := `{"x": "` + otherBox.Seal("bad") + `"}`
	out, changed, failed = bs.ResealText(bad)
	if changed != 0 || failed != 1 || out != bad {
		t.Fatalf("resealText unopenable: changed=%d failed=%d out=%q", changed, failed, out)
	}
	// Plaintext without tokens is untouched.
	if out, changed, failed := bs.ResealText("nothing here"); out != "nothing here" || changed != 0 || failed != 0 {
		t.Fatalf("resealText plaintext: %q %d %d", out, changed, failed)
	}
}

func TestBoxSetWithKey(t *testing.T) {
	bs, _ := NewBoxSet("new-key-0123456789", "old-key-0123456789")
	otherBox, _ := New("third-key-0123456789")
	foreign := otherBox.Seal("foreign")
	if _, err := bs.Open(foreign); err == nil {
		t.Fatal("foreign value opened without its key")
	}
	// Adding the source key makes it openable, and the original set is
	// unchanged (WithKey returns a new set).
	with := bs.WithKey("third-key-0123456789")
	if got, err := with.Open(foreign); err != nil || got != "foreign" {
		t.Fatalf("WithKey open: %q %v", got, err)
	}
	if _, err := bs.Open(foreign); err == nil {
		t.Fatal("WithKey mutated the original set")
	}
	// Sealing through the extended set still uses the current key.
	if sealed := with.Seal("z"); !strings.HasPrefix(sealed, "enc:v2:"+bs.CurrentKid()+":") {
		t.Fatalf("WithKey seal used the wrong key: %q", sealed)
	}
}

func TestNewBoxSetValidation(t *testing.T) {
	if _, err := NewBoxSet("", ""); err == nil {
		t.Fatal("empty current key accepted")
	}
	if _, err := NewBoxSet("k", "k"); err == nil {
		t.Fatal("previous == current accepted")
	}
	bs, err := NewBoxSet("only-key-0123456789", "")
	if err != nil {
		t.Fatal(err)
	}
	if bs.HasPrevious() || bs.PreviousKid() != "" {
		t.Fatal("single-key set reports a previous key")
	}
	if !bs.KnowsKid(bs.CurrentKid()) || bs.KnowsKid("deadbeef") {
		t.Fatal("KnowsKid wrong")
	}
}
