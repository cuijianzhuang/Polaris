package cdnkit

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestFailureSlotAndCheckKey(t *testing.T) {
	if FailureSlot("ep", "1.2.3.4") != FailureSlot("ep", "1.2.3.4") {
		t.Fatal("slot not stable")
	}
	if FailureSlot("ep", "1.2.3.4") == FailureSlot("ep", "1.2.3.5") {
		t.Fatal("slot ignores address")
	}
	target := Target{Server: "1.2.3.4", Port: 443}
	if CheckKey("ep", target, model.TransWS, "r1") == CheckKey("ep", target, model.TransWS, "r2") {
		t.Fatal("check key ignores revision")
	}
}

func TestHealthRevision(t *testing.T) {
	row := store.Inbound{Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecNone})}
	p := &store.CDNProfile{TLSMode: "full"}
	a := HealthRevision(row, p)
	p.TLSMode = "flexible"
	if HealthRevision(row, p) == a {
		t.Fatal("revision ignores TLS mode")
	}
	p.TLSMode = "full"
	p.OriginMode = "tunnel"
	p.TunnelID = "x"
	if HealthRevision(row, p) == a {
		t.Fatal("revision ignores tunnel")
	}
}

func profileWithChecks() *store.CDNProfile {
	failed, _ := json.Marshal(AddressResult{
		Check:      Check{Message: "down"},
		Target:     Target{Server: "1.2.3.4"},
		EndpointID: "ep",
		CheckedAt:  time.Now().Add(-time.Hour),
	})
	ok, _ := json.Marshal(AddressResult{
		Check:      Check{OK: true},
		Target:     Target{Server: "5.6.7.8"},
		EndpointID: "ep",
		CheckedAt:  time.Now(),
	})
	return &store.CDNProfile{
		AddressChecks: store.NewJSON(map[string]json.RawMessage{"k-fail": failed, "k-ok": ok}),
	}
}

func TestCachedAddressCheck(t *testing.T) {
	p := profileWithChecks()
	r := CachedAddressCheck(p, "k-fail")
	if r == nil || r.OK || r.Target.Server != "1.2.3.4" {
		t.Fatalf("cached fail: %+v", r)
	}
	if !CheckFailed(r) || CheckFailed(CachedAddressCheck(p, "k-ok")) || CheckFailed(nil) {
		t.Fatal("CheckFailed")
	}
	if CachedAddressCheck(p, "missing") != nil {
		t.Fatal("missing check decoded")
	}
	p.AddressChecks = store.NewJSON(map[string]json.RawMessage{"bad": json.RawMessage("{not json")})
	if CachedAddressCheck(p, "bad") != nil {
		t.Fatal("corrupt check decoded")
	}
}

func TestAddressFailureAtAndVerdict(t *testing.T) {
	now := time.Now()
	target := Target{Server: "1.2.3.4", Port: 443}
	tr := model.TransWS
	p := &store.CDNProfile{}
	key := CheckKey("ep", target, tr, "r1")
	// No failure recorded yet.
	if AddressFailureAt(p, "ep", target, tr, "r1") != nil {
		t.Fatal("phantom failure")
	}
	// A failed verdict records the slot.
	RecordAddressVerdict(p, key, AddressResult{Check: Check{Message: "down"}, Target: target, EndpointID: "ep", CheckedAt: now})
	at := AddressFailureAt(p, "ep", target, tr, "r1")
	if at == nil || !at.Equal(now) {
		t.Fatalf("failure not recorded: %v", at)
	}
	// A partial result cannot clear it.
	RecordAddressVerdict(p, key, AddressResult{Check: Check{Partial: true, Message: "partial"}, Target: target, EndpointID: "ep", CheckedAt: now.Add(time.Minute)})
	if AddressFailureAt(p, "ep", target, tr, "r1") == nil {
		t.Fatal("partial cleared a failure")
	}
	// A successful recheck clears it.
	RecordAddressVerdict(p, key, AddressResult{Check: Check{OK: true}, Target: target, EndpointID: "ep", CheckedAt: now.Add(2 * time.Minute)})
	if AddressFailureAt(p, "ep", target, tr, "r1") != nil {
		t.Fatal("success did not clear the failure")
	}
	// A failure under another revision does not match.
	RecordAddressVerdict(p, key, AddressResult{Check: Check{Message: "down"}, Target: target, EndpointID: "ep", CheckedAt: now})
	if AddressFailureAt(p, "ep", target, tr, "r2") != nil {
		t.Fatal("failure matched a different revision")
	}
}

func TestRecordAddressVerdictPromotesHistory(t *testing.T) {
	// A failed detail record promotes into the compact set even when the
	// verdict being recorded is for another endpoint.
	p := profileWithChecks()
	RecordAddressVerdict(p, "k-other", AddressResult{
		Check: Check{OK: true}, Target: Target{Server: "9.9.9.9"},
		EndpointID: "other", CheckedAt: time.Now(),
	})
	slot := FailureSlot("ep", "1.2.3.4")
	if _, ok := p.AddressFailures.V[slot]; !ok {
		t.Fatal("historical failure not promoted")
	}
}
