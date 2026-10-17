package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestParse(t *testing.T) {
	pub, priv := keypair(t)
	oldPub, oldPriv := keypair(t)
	l := &License{ID: "lic_1", Customer: "ACME", Plan: "pro", Limits: Limits{Nodes: 20, Users: 2000},
		Features: []string{FeatureSpeedLimit}, Instance: "inst_a", IssuedAt: "2026-10-17", ExpiresAt: "2027-10-17"}
	text, err := Sign(l, priv)
	if err != nil {
		t.Fatal(err)
	}
	keys := []ed25519.PublicKey{pub, oldPub}

	got, err := Parse(text, keys, "inst_a")
	if err != nil || got.Plan != "pro" || got.Limits.Nodes != 20 || !got.Has(FeatureSpeedLimit) {
		t.Fatalf("parse: %+v %v", got, err)
	}
	// Line breaks from copy and paste are fine.
	if _, err := Parse(text[:20]+"\n"+text[20:]+"\n", keys, "inst_a"); err != nil {
		t.Fatalf("with line breaks: %v", err)
	}
	// A rotated (second) key is accepted too.
	text2, _ := Sign(l, oldPriv)
	if _, err := Parse(text2, keys, "inst_a"); err != nil {
		t.Fatalf("second key: %v", err)
	}
	// Another instance, another signer, tampering, garbage.
	if _, err := Parse(text, keys, "inst_b"); !errors.Is(err, ErrInstance) {
		t.Fatalf("other instance: %v", err)
	}
	_, strange := keypair(t)
	text3, _ := Sign(l, strange)
	if _, err := Parse(text3, keys, "inst_a"); !errors.Is(err, ErrSignature) {
		t.Fatalf("foreign signer: %v", err)
	}
	body, sig, _ := strings.Cut(text, ".")
	raw, _ := b64.DecodeString(body)
	tampered := strings.Replace(string(raw), `"nodes":20`, `"nodes":99`, 1)
	if _, err := Parse(b64.EncodeToString([]byte(tampered))+"."+sig, keys, "inst_a"); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := Parse("nonsense", keys, ""); !errors.Is(err, ErrFormat) {
		t.Fatalf("garbage: %v", err)
	}
	if _, err := Parse(text, nil, "inst_a"); !errors.Is(err, ErrSignature) {
		t.Fatalf("no keys (development build): %v", err)
	}
}

func TestParseKeys(t *testing.T) {
	a, _ := keypair(t)
	b, _ := keypair(t)
	ks := ParseKeys(base64.StdEncoding.EncodeToString(a) + ", " + base64.RawURLEncoding.EncodeToString(b) + ",junk")
	if len(ks) != 2 || !ks[0].Equal(a) || !ks[1].Equal(b) {
		t.Fatalf("keys %v", ks)
	}
}

func TestEvaluate(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	lic := &License{ID: "l", Plan: "pro", Limits: Limits{Nodes: 20, Users: 2000}, Features: []string{FeatureSpeedLimit, FeatureBackupPush},
		ExpiresAt: "2027-10-17", UpdatesUntil: "2027-01-01"}

	st := Evaluate(Inputs{Now: now})
	if st.Plan != "community" || st.Allow(FeatureSpeedLimit) || st.Limit(LimitNodes) != Community.Nodes {
		t.Fatalf("community %+v", st)
	}
	st = Evaluate(Inputs{License: lic, Now: now, LastCheck: now})
	if st.Plan != "pro" || !st.Allow(FeatureBackupPush) || st.Allow(FeatureTGMenu) || st.Limit(LimitUsers) != 2000 || len(st.Warnings) != 0 {
		t.Fatalf("licensed %+v", st)
	}
	// Expiry day still counts; the day after falls back.
	if st := Evaluate(Inputs{License: lic, Now: time.Date(2027, 10, 17, 23, 0, 0, 0, time.UTC)}); st.Plan != "pro" {
		t.Fatalf("on expiry day %+v", st)
	}
	if st := Evaluate(Inputs{License: lic, Now: time.Date(2027, 10, 19, 0, 0, 0, 0, time.UTC)}); st.Plan != "community" || len(st.Warnings) == 0 {
		t.Fatalf("expired %+v", st)
	}
	if st := Evaluate(Inputs{License: lic, Now: now, Revoked: true}); st.Plan != "community" {
		t.Fatalf("revoked %+v", st)
	}
	// A build released after the update period is not covered.
	if st := Evaluate(Inputs{License: lic, Now: now, Released: time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)}); st.Plan != "community" {
		t.Fatalf("release after updates_until %+v", st)
	}
	// Server unreachable for 15 days: still licensed, with a warning.
	if st := Evaluate(Inputs{License: lic, Now: now, LastCheck: now.Add(-15 * 24 * time.Hour)}); st.Plan != "pro" || len(st.Warnings) != 1 {
		t.Fatalf("offline too long %+v", st)
	}
	// Trial unlocks everything until it ends, and applies after an expired license.
	trial := now.Add(10 * 24 * time.Hour)
	if st := Evaluate(Inputs{Now: now, TrialUntil: trial}); st.Plan != "trial" || !st.Allow(FeatureTGMenu) {
		t.Fatalf("trial %+v", st)
	}
	if st := Evaluate(Inputs{Now: trial.Add(time.Minute), TrialUntil: trial}); st.Plan != "community" {
		t.Fatalf("trial over %+v", st)
	}
}

func TestEvaluateUnenforced(t *testing.T) {
	st := Evaluate(Inputs{Now: time.Now(), Unenforced: true})
	if st.Plan != "open" || !st.Allow(FeatureTGMenu) {
		t.Fatalf("%+v", st)
	}
}

func TestEveryFeature(t *testing.T) {
	l := &License{Features: []string{FeatureSpeedLimit, "future_thing"}}
	if l.AllGranted() || len(l.Granted()) != 1 || !l.Has(FeatureSpeedLimit) || l.Has(FeatureDDNS) {
		t.Fatalf("partial %v", l.Granted())
	}
	l.Features = []string{FeatureSpeedLimit, EveryFeature}
	if !l.AllGranted() || len(l.Granted()) != len(AllFeatures) || !l.Has(FeatureDDNS) {
		t.Fatalf("every %v", l.Granted())
	}
	st := Evaluate(Inputs{License: l, Now: time.Now()})
	if !st.Allow(FeatureDDNS) || !st.Allow(FeatureSubConvert) {
		t.Fatalf("state %v", st.Features)
	}
}
