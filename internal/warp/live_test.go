package warp

import (
	"os"
	"testing"
)

// TestLiveRegister registers a real free device (POLARIS_WARP_LIVE=1).
func TestLiveRegister(t *testing.T) {
	if os.Getenv("POLARIS_WARP_LIVE") == "" {
		t.Skip("POLARIS_WARP_LIVE not set")
	}
	d, err := Register(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if d.PeerPublicKey == "" || len(d.LocalAddresses()) == 0 || len(d.Reserved) != 3 || d.Endpoint == "" {
		t.Fatalf("incomplete device: %+v", d)
	}
	t.Logf("endpoint %s, addresses %v, reserved %v, account %s", d.Endpoint, d.LocalAddresses(), d.Reserved, d.AccountType)
}
