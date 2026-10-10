package settings

import "testing"

func TestIsSecretKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"sso_client_secret", true},
		{"backup_s3_secret_key", true},
		{"sso_client_password", true},
		{"api_token", true},
		{"openobserve_credentials", true},
		{"SSO_CLIENT_SECRET", true}, // case-insensitive
		{"domain", false},
		{"volume_root", false},
		{"backup_all_nodes", false},
		{"sso_enabled", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsSecretKey(tc.key); got != tc.want {
			t.Errorf("IsSecretKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestMaskValue(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		value   string
		isAdmin bool
		want    string
	}{
		{"non-admin secret", "sso_client_secret", "hunter2", false, Mask},
		{"non-admin key", "backup_s3_access_key", "ak", false, Mask},
		{"admin secret", "sso_client_secret", "hunter2", true, "hunter2"},
		{"admin key", "backup_s3_access_key", "ak", true, "ak"},
		{"non-admin plain", "domain", "example.com", false, "example.com"},
		{"empty value", "sso_client_secret", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaskValue(tc.key, tc.value, tc.isAdmin); got != tc.want {
				t.Errorf("MaskValue(%q, %q, %v) = %q, want %q", tc.key, tc.value, tc.isAdmin, got, tc.want)
			}
		})
	}
}
