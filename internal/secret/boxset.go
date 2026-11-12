package secret

// BoxSet holds the current panel secret key plus, during a rotation, the
// previous one (and, when restoring a backup, the source panel's key).
// It exposes the same Seal / Open / MustOpen signatures as Box, so call
// sites do not change: only the construction site and the Store.Box field
// type know a set exists.
//
// Key ids are derived deterministically from the passphrase — the first 8
// hex characters of SHA256("kid:"+passphrase) — so a BoxSet can be built
// without reading the database. The id is not secret; it only selects
// which key opens a value. New values are always sealed with the current
// key as enc:v2:<kid>:<base64(nonce‖ciphertext)>; enc:v1: values (the
// pre-rotation format, no id) are tried with the previous key first, then
// the current key. An unknown kid is an error, never a plaintext fallback.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	v1Prefix = "enc:v1:"
	v2Prefix = "enc:v2:"
)

// SealedValue matches any sealed token, v1 or v2, embedded in a larger
// string (a JSON document, a settings value, a backup table dump).
var SealedValue = regexp.MustCompile(`enc:v2:[0-9a-f]{8}:[A-Za-z0-9+/]+|enc:v1:[A-Za-z0-9+/]+`)

// KidFor derives the key id for a passphrase. The derivation is stable
// across processes, so a kid written into a sealed value can be matched
// against a configured key without any database lookup.
func KidFor(passphrase string) string {
	sum := sha256.Sum256([]byte("kid:" + passphrase))
	return hex.EncodeToString(sum[:])[:8]
}

// IsSealed reports whether s is exactly one sealed value (v1 or v2).
func IsSealed(s string) bool {
	return strings.HasPrefix(s, v1Prefix) || strings.HasPrefix(s, v2Prefix)
}

// ContainsSealed reports whether s contains a sealed token anywhere.
func ContainsSealed(s string) bool {
	return strings.Contains(s, v1Prefix) || strings.Contains(s, v2Prefix)
}

// BoxSet is a set of Box keys keyed by their derived kid.
type BoxSet struct {
	boxes      map[string]*Box
	tryOrder   []string // kids tried for enc:v1: values, previous first
	currentKid string
	prevKid    string
}

// NewBoxSet builds a set from the current passphrase and, during a
// rotation, the previous one ("" when there is no previous key). It is an
// error to pass the same passphrase twice: previous == current means the
// rotation has nothing to rotate from, and silently accepting it would
// hide a misconfiguration.
func NewBoxSet(currentPass, previousPass string) (*BoxSet, error) {
	if currentPass == "" {
		return nil, errors.New("secret key is empty")
	}
	if previousPass != "" && previousPass == currentPass {
		return nil, errors.New("secret_key_previous is the same as secret_key")
	}
	bs := &BoxSet{boxes: map[string]*Box{}, currentKid: KidFor(currentPass)}
	current, err := New(currentPass)
	if err != nil {
		return nil, err
	}
	bs.boxes[bs.currentKid] = current
	bs.tryOrder = []string{bs.currentKid}
	if previousPass != "" {
		prev, err := New(previousPass)
		if err != nil {
			return nil, err
		}
		bs.prevKid = KidFor(previousPass)
		bs.boxes[bs.prevKid] = prev
		// v1 values carry no kid: the previous key sealed most of them,
		// so try it first.
		bs.tryOrder = []string{bs.prevKid, bs.currentKid}
	}
	return bs, nil
}

// WithKey returns a set that additionally knows passphrase (used when
// restoring a backup whose source key is supplied explicitly). The
// receiver is not modified; the new set shares the underlying boxes.
func (s *BoxSet) WithKey(passphrase string) *BoxSet {
	if passphrase == "" {
		return s
	}
	kid := KidFor(passphrase)
	if _, ok := s.boxes[kid]; ok {
		return s
	}
	box, err := New(passphrase)
	if err != nil {
		return s
	}
	boxes := make(map[string]*Box, len(s.boxes)+1)
	for k, v := range s.boxes {
		boxes[k] = v
	}
	boxes[kid] = box
	tryOrder := append(append([]string{}, s.tryOrder...), kid)
	return &BoxSet{boxes: boxes, tryOrder: tryOrder, currentKid: s.currentKid, prevKid: s.prevKid}
}

// CurrentKid is the kid new values are sealed with.
func (s *BoxSet) CurrentKid() string { return s.currentKid }

// PreviousKid is the kid of the previous key, or "" when there is none.
func (s *BoxSet) PreviousKid() string { return s.prevKid }

// HasPrevious reports whether a previous key is configured.
func (s *BoxSet) HasPrevious() bool { return s.prevKid != "" }

// KnowsKid reports whether the set can open values sealed with kid.
func (s *BoxSet) KnowsKid(kid string) bool {
	_, ok := s.boxes[kid]
	return ok
}

// Seal encrypts plaintext with the current key as enc:v2:<kid>:.
// An already-sealed value is re-sealed with the current key when it can
// be opened (this is what converges old rows onto the current key); a
// value that cannot be opened is returned unchanged rather than
// destroyed. Empty strings stay empty.
func (s *BoxSet) Seal(plain string) string {
	if plain == "" {
		return plain
	}
	if IsSealed(plain) {
		if next, err := s.Reseal(plain); err == nil {
			return next
		}
		return plain
	}
	return s.sealCurrent(plain)
}

func (s *BoxSet) sealCurrent(plain string) string {
	b := s.boxes[s.currentKid]
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	ct := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return v2Prefix + s.currentKid + ":" + base64.RawStdEncoding.EncodeToString(ct)
}

// Reseal returns the current-key form of a sealed value: unchanged when
// it is already sealed with the current key, re-sealed otherwise.
// Plaintext is returned unchanged — the migration must never encrypt
// rows that predate encryption. An unopenable value is an error.
func (s *BoxSet) Reseal(token string) (string, error) {
	if token == "" || !IsSealed(token) {
		return token, nil
	}
	if strings.HasPrefix(token, v2Prefix) {
		if kid, ok := v2Kid(token); ok && kid == s.currentKid {
			return token, nil
		}
	}
	pt, err := s.Open(token)
	if err != nil {
		return "", err
	}
	return s.sealCurrent(pt), nil
}

// ResealText replaces every sealed token in text with its current-key
// form and returns the new text, how many tokens changed, and how many
// tokens could not be opened (left unchanged). Plaintext is untouched.
func (s *BoxSet) ResealText(text string) (string, int, int) {
	changed, failed := 0, 0
	out := SealedValue.ReplaceAllStringFunc(text, func(m string) string {
		next, err := s.Reseal(m)
		if err != nil {
			failed++
			return m
		}
		if next != m {
			changed++
		}
		return next
	})
	return out, changed, failed
}

// Open decrypts a sealed string. enc:v2: values select their box by kid;
// enc:v1: values are tried with the previous key first, then the current
// key (and any extra keys, in the order they were added). Unsealed input
// is returned unchanged, as with Box. An unknown kid is an error.
func (s *BoxSet) Open(v string) (string, error) {
	if strings.HasPrefix(v, v2Prefix) {
		kid, raw, ok := parseV2(v)
		if !ok {
			return "", errors.New("malformed enc:v2 value")
		}
		b, ok := s.boxes[kid]
		if !ok {
			return "", fmt.Errorf("unknown key id %q", kid)
		}
		return openBox(b, raw)
	}
	if strings.HasPrefix(v, v1Prefix) {
		raw := strings.TrimPrefix(v, v1Prefix)
		var lastErr error
		for _, kid := range s.tryOrder {
			if pt, err := openBox(s.boxes[kid], raw); err == nil {
				return pt, nil
			} else {
				lastErr = err
			}
		}
		if lastErr != nil {
			return "", lastErr
		}
		return "", errors.New("no key can open this value")
	}
	return v, nil
}

// MustOpen decrypts or returns an empty string on failure.
func (s *BoxSet) MustOpen(v string) string {
	out, err := s.Open(v)
	if err != nil {
		return ""
	}
	return out
}

// parseV2 splits "enc:v2:<kid>:<base64>" into kid and payload.
func parseV2(v string) (kid, raw string, ok bool) {
	rest := strings.TrimPrefix(v, v2Prefix)
	kid, raw, found := strings.Cut(rest, ":")
	if !found || len(kid) != 8 {
		return "", "", false
	}
	for _, c := range kid {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", "", false
		}
	}
	return kid, raw, true
}

// v2Kid returns the kid of an enc:v2: value, or "" when malformed.
func v2Kid(v string) (string, bool) {
	kid, _, ok := parseV2(v)
	return kid, ok
}

func openBox(b *Box, raw string) (string, error) {
	data, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return "", err
	}
	n := b.aead.NonceSize()
	if len(data) < n {
		return "", errors.New("ciphertext too short")
	}
	pt, err := b.aead.Open(nil, data[:n], data[n:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
