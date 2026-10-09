package controllers

import (
	"strings"
	"testing"
)

// TestPasswordTooLong pins the bcrypt 72-byte boundary used to refuse overly
// long passwords at user creation / password change.
func TestPasswordTooLong(t *testing.T) {
	if PasswordTooLong("short") {
		t.Error("short password flagged as too long")
	}
	if !PasswordTooLong(strings.Repeat("a", 73)) {
		t.Error("73-byte password not flagged as too long")
	}
	if PasswordTooLong(strings.Repeat("a", 72)) {
		t.Error("exactly-72-byte password flagged as too long (limit is inclusive)")
	}
}

// TestHashPassword_RejectsOver72Bytes verifies hashPassword refuses a password
// beyond the bcrypt limit with a clear error instead of silently truncating.
func TestHashPassword_RejectsOver72Bytes(t *testing.T) {
	if _, err := hashPassword(strings.Repeat("a", 73)); err == nil {
		t.Fatal("hashPassword(73 bytes) succeeded, want error")
	}
	if _, err := hashPassword(strings.Repeat("a", 72)); err != nil {
		t.Fatalf("hashPassword(72 bytes) = %v, want success", err)
	}
}

// TestValidateNewAdmin_RejectsOver72Bytes verifies the first-run setup flow
// reports the too-long rule with the dedicated i18n key.
func TestValidateNewAdmin_RejectsOver72Bytes(t *testing.T) {
	p := strings.Repeat("a", 73)
	key, bad := validateNewAdmin("admin", p, p)
	if !bad || key != "auth.err.password_too_long" {
		t.Fatalf("validateNewAdmin = (%q, %v), want (auth.err.password_too_long, true)", key, bad)
	}
}
