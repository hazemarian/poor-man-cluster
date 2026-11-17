package docker

import "testing"

func TestMountName(t *testing.T) {
	for in, want := range map[string]string{
		"aarma_db_password_43dd4a0c":  "aarma_db_password",
		"aarma_admin_phones_0318b337": "aarma_admin_phones",
		"plain":                       "plain",
		"trailing_zzzzzzzz":           "trailing_zzzzzzzz", // not hex: keep as-is
	} {
		if got := mountName(in); got != want {
			t.Errorf("mountName(%q) = %q, want %q", in, got, want)
		}
	}
}
