// Package buildinfo_test exercises the Resolve() function.
//
// Limitation: the VCS-info fallback path (debug.ReadBuildInfo returning
// settings with "vcs.revision" / "vcs.time") cannot be exercised in unit tests
// because debug.ReadBuildInfo() does not provide VCS info when tests are run
// via `go test` (only when the binary is built with `go build`). The fallback
// branch is therefore left untested here; a manual smoke test (build with
// `make build`, run `pmcluster version`) confirms it works.
package buildinfo

import (
	"testing"
)

// TestResolve_VarsSet verifies that when the package-level ldflags targets are
// non-default values, Resolve() returns them directly without VCS-info lookup.
func TestResolve_VarsSet(t *testing.T) {

	origVersion := Version
	origCommit := Commit
	origDate := Date
	t.Cleanup(func() {
		Version = origVersion
		Commit = origCommit
		Date = origDate
	})

	Version = "v1.2.3"
	Commit = "abc1234"
	Date = "2026-05-10T00:00:00Z"

	v, c, d := Resolve()
	if v != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", v)
	}
	if c != "abc1234" {
		t.Errorf("commit = %q, want abc1234", c)
	}
	if d != "2026-05-10T00:00:00Z" {
		t.Errorf("date = %q, want 2026-05-10T00:00:00Z", d)
	}
}

// TestResolve_DefaultsReturnSomething verifies that even without ldflags
// injection, Resolve() never returns empty strings — it at least returns
// the compiled-in defaults ("dev", "unknown", "unknown").
func TestResolve_DefaultsReturnSomething(t *testing.T) {

	v, c, d := Resolve()
	if v == "" {
		t.Error("Resolve() returned empty version")
	}
	if c == "" {
		t.Error("Resolve() returned empty commit")
	}
	if d == "" {
		t.Error("Resolve() returned empty date")
	}
}

// TestResolve_NonDevVersionSkipsVCSFallback verifies that if Version is
// something other than "dev" (as would be the case for a release build),
// Resolve() returns immediately without touching debug.ReadBuildInfo.
func TestResolve_NonDevVersionSkipsVCSFallback(t *testing.T) {
	origVersion := Version
	origCommit := Commit
	origDate := Date
	t.Cleanup(func() {
		Version = origVersion
		Commit = origCommit
		Date = origDate
	})

	Version = "v2.0.0"
	Commit = "deadbeef"
	Date = "2026-01-01T00:00:00Z"

	v, c, d := Resolve()

	if v != "v2.0.0" || c != "deadbeef" || d != "2026-01-01T00:00:00Z" {
		t.Errorf("Resolve() = (%q, %q, %q), want (v2.0.0, deadbeef, 2026-01-01T00:00:00Z)", v, c, d)
	}
}
