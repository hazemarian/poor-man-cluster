package cluster

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// withCredentialHelper swaps the workflow seam and returns a pointer to the call
// count, the same shape as withPlatformHostDirs.
func withCredentialHelper(t *testing.T, fn func() error) *int {
	t.Helper()
	orig := ensureDockerCredentialHelper
	t.Cleanup(func() { ensureDockerCredentialHelper = orig })
	calls := 0
	ensureDockerCredentialHelper = func() error {
		calls++
		return fn()
	}
	return &calls
}

// TestUpInstallsTheCredentialHelper: the workflow must run it, not just have it
// available — a step nobody calls is the bug this asserts against.
func TestUpInstallsTheCredentialHelper(t *testing.T) {
	deps, _, _ := newUpDeps(t)
	calls := withCredentialHelper(t, func() error { return nil })
	var buf bytes.Buffer
	deps.Stdout = &buf

	dir := t.TempDir()
	in := UpInput{
		Domain:                  "test.example.com",
		CertPath:                writeTempFile(t, dir, "cert.pem", []byte("CERT")),
		KeyPath:                 writeTempFile(t, dir, "key.pem", []byte("KEY")),
		ObservabilityAdminEmail: "ops@example.com",
	}
	if _, err := Up(context.Background(), deps, in); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if *calls != 1 {
		t.Errorf("the credential helper ran %d times during cluster up, want 1", *calls)
	}
	if !strings.Contains(buf.String(), CredHelperHost) {
		t.Errorf("up output does not report the helper:\n%s", buf.String())
	}
}

// TestUpWarnsButContinuesWhenTheHelperCannotBeInstalled: provisioning a host
// path under /root and /usr/local/bin cannot succeed on a non-root host, and a
// cluster that pulls public images is healthy without it. Aborting the bring-up
// there would be a worse failure than the one being reported.
func TestUpWarnsButContinuesWhenTheHelperCannotBeInstalled(t *testing.T) {
	deps, _, deployer := newUpDeps(t)
	withCredentialHelper(t, func() error { return errors.New("create /usr/local/bin: permission denied") })
	var buf bytes.Buffer
	deps.Stdout = &buf

	dir := t.TempDir()
	in := UpInput{
		Domain:                  "test.example.com",
		CertPath:                writeTempFile(t, dir, "cert.pem", []byte("CERT")),
		KeyPath:                 writeTempFile(t, dir, "key.pem", []byte("KEY")),
		ObservabilityAdminEmail: "ops@example.com",
	}
	if _, err := Up(context.Background(), deps, in); err != nil {
		t.Fatalf("Up aborted on a credential-helper failure: %v", err)
	}
	if !strings.Contains(buf.String(), "permission denied") {
		t.Errorf("the warning does not carry the cause:\n%s", buf.String())
	}
	if len(deployer.deployedStacks) == 0 {
		t.Error("no stack was deployed after the helper warning")
	}
}

// TestUpdateWarnsButContinuesWhenTheHelperCannotBeInstalled: same rule on the
// reconcile path, where every degradation is a warning line.
func TestUpdateWarnsButContinuesWhenTheHelperCannotBeInstalled(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	withCredentialHelper(t, func() error { return errors.New("create /usr/local/bin: permission denied") })
	var buf bytes.Buffer
	deps.Stdout = &buf

	if _, err := Update(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0", Force: true}); err != nil {
		t.Fatalf("Update aborted on a credential-helper failure: %v", err)
	}
	if !strings.Contains(buf.String(), "permission denied") {
		t.Errorf("the warning does not carry the cause:\n%s", buf.String())
	}
}
