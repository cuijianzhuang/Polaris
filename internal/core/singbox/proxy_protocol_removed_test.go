package singbox

import (
	"encoding/json"
	"testing"
)

// TestStripRemovedProxyProtocolFromExtra covers recovery of stale saved
// configs: a raw Extra pasted from Xray or an older sing-box version must be
// stripped before render so the core does not abort at startup with
// "Proxy Protocol is deprecated and removed in sing-box 1.6.0".
func TestStripRemovedProxyProtocolFromExtra(t *testing.T) {
	for _, key := range removedProxyProtocolKeys {
		extra := json.RawMessage(`{"` + key + `":true,"sniffing":{"routeOnly":true}}`)
		cleaned := stripRemovedProxyProtocol(extra)
		var obj map[string]any
		if err := json.Unmarshal(cleaned, &obj); err != nil {
			t.Fatal(err)
		}
		if _, ok := obj[key]; ok {
			t.Fatalf("%s survived strip", key)
		}
		if _, ok := obj["sniffing"]; !ok {
			t.Fatal("unrelated Extra keys were dropped")
		}
		if err := CheckInboundExtra(extra); err == nil {
			t.Fatalf("CheckInboundExtra accepted removed key %s", key)
		}
	}
	// Clean Extra is untouched and passes validation.
	clean := json.RawMessage(`{"sniffing":{"routeOnly":true}}`)
	if got := stripRemovedProxyProtocol(clean); string(got) != string(clean) {
		t.Fatalf("clean Extra modified: %s", got)
	}
	if err := CheckInboundExtra(clean); err != nil {
		t.Fatal(err)
	}
	// Invalid/empty Extra is ignored.
	for _, extra := range []json.RawMessage{nil, []byte(""), []byte("not json")} {
		_ = stripRemovedProxyProtocol(extra)
		_ = CheckInboundExtra(extra)
	}
}
