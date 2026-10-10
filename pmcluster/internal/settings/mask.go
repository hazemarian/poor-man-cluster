package settings

import "strings"

// Mask is the fixed placeholder substituted for secret-ish values shown to
// non-admin callers.
const Mask = "********"

// IsSecretKey reports whether a setting key names secret material. A key is
// secret-ish when its lowercased name contains any of "secret", "password",
// "key", "token", or "credential". This is the single policy shared by the
// settings HTTP handler, the CLI printer, and the console cluster-settings
// form.
func IsSecretKey(key string) bool {
	lk := strings.ToLower(key)
	for _, sub := range []string{"secret", "password", "key", "token", "credential"} {
		if strings.Contains(lk, sub) {
			return true
		}
	}
	return false
}

// MaskValue returns the value to display for a setting. Empty values stay
// empty; admins see the cleartext value; non-admins see Mask for secret-ish
// keys and cleartext for everything else.
func MaskValue(key, value string, isAdmin bool) string {
	if value == "" || isAdmin || !IsSecretKey(key) {
		return value
	}
	return Mask
}
