package panel

import (
	"crypto/ed25519"
	"time"
)

// licenseAll unlocks every paid feature for tests of those features.
func licenseAll(s *Server) {
	s.st.SetSetting("license_trial_until", time.Now().Add(time.Hour).Format(time.RFC3339))
	s.reloadLicense()
}

// licenseNone runs a test as an unlicensed panel, also in pro builds (which
// would otherwise run unenforced without license keys).
func licenseNone(s *Server) {
	pub, _, _ := ed25519.GenerateKey(nil)
	s.licKeys = []ed25519.PublicKey{pub}
	s.st.SetSetting("license_trial_until", "")
	s.reloadLicense()
}
