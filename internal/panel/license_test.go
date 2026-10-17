package panel

import "time"

// licenseAll unlocks every paid feature for tests of those features.
func licenseAll(s *Server) {
	s.st.SetSetting("license_trial_until", time.Now().Add(time.Hour).Format(time.RFC3339))
	s.reloadLicense()
}
