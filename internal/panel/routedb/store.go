package routedb

// SettingStore is the database behind a stored route policy: one
// settings row by key. The SQL lives with the caller; only the policy
// encoding lives here.
type SettingStore interface {
	// Setting returns the value of a settings row, and whether it exists.
	Setting(key string) (value string, found bool, err error)
}

// ReadPolicy reads and decodes the stored policy for key. A missing row
// is the default policy (disabled, no snapshot). A malformed value
// reports the caller-supplied sentinel error.
func ReadPolicy(st SettingStore, key string, invalid error) (Policy, error) {
	value, found, err := st.Setting(key)
	if err != nil {
		return Policy{}, err
	}
	if !found {
		return Policy{}, nil
	}
	return DecodePolicy(value, invalid)
}
