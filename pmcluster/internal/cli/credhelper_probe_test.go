package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeHelper lays out an executable that behaves like the real helper's
// CLI surface: it reads stdin and writes to stdout/stderr.
func writeFakeHelper(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker-credential-pmcluster")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}
	return path
}

// TestCredHelperProbeReadsTheHelperOutput: `doctor` runs the same command docker
// runs and reads the same JSON on stdout.
func TestCredHelperProbeReadsTheHelperOutput(t *testing.T) {
	// The fake records the verb and stdin so the test also pins the protocol
	// shape (`get` + the registry on stdin).
	var stdinFile = filepath.Join(t.TempDir(), "stdin")
	path := writeFakeHelper(t, `cat > `+stdinFile+`
printf '{"ServerURL":"ghcr.io","Username":"x-access-token","Secret":"ghs_probe_test"}\n'`)

	secret, err := credHelperProbe{Path: path}.GHCRToken(context.Background())
	if err != nil {
		t.Fatalf("GHCRToken: %v", err)
	}
	if secret != "ghs_probe_test" {
		t.Errorf("secret = %q, want the helper's token", secret)
	}
	got, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("read recorded stdin: %v", err)
	}
	if strings.TrimSpace(string(got)) != "ghcr.io" {
		t.Errorf("the helper received %q on stdin, want the registry", got)
	}
}

// TestCredHelperProbeSurfacesTheHelperStderr: the helper's refusal is the
// actionable half of a failing check (it says whether the edge is down or the
// certificate was refused), so it must survive into the report.
func TestCredHelperProbeSurfacesTheHelperStderr(t *testing.T) {
	path := writeFakeHelper(t, `echo "reach the credential listener at https://127.0.0.1:9443: connection refused" >&2
exit 1`)

	_, err := credHelperProbe{Path: path}.GHCRToken(context.Background())
	if err == nil {
		t.Fatal("GHCRToken succeeded against a failing helper")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %v, want the helper's stderr", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name the helper binary", err)
	}
}

// TestCredHelperProbeRejectsAnEmptyOrUnparsableAnswer: a helper that exits 0
// with nothing useful is a broken helper, not a credential.
func TestCredHelperProbeRejectsAnEmptyOrUnparsableAnswer(t *testing.T) {
	empty := writeFakeHelper(t, `cat >/dev/null
printf '{"ServerURL":"ghcr.io","Username":"x-access-token","Secret":""}\n'`)
	if _, err := (credHelperProbe{Path: empty}).GHCRToken(context.Background()); err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Errorf("empty token: %v, want an error naming it", err)
	}

	garbage := writeFakeHelper(t, `cat >/dev/null
printf 'not json\n'`)
	if _, err := (credHelperProbe{Path: garbage}).GHCRToken(context.Background()); err == nil {
		t.Error("non-JSON output was accepted")
	}
}

// TestCredHelperProbeNamesAMissingHelper: the most common failure is "nobody
// installed it on this node", and the message has to say how to.
func TestCredHelperProbeNamesAMissingHelper(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing named docker-credential-pmcluster
	_, err := credHelperProbe{}.GHCRToken(context.Background())
	if err == nil {
		t.Fatal("GHCRToken found a helper on an empty PATH")
	}
	if !strings.Contains(err.Error(), "pmcluster cluster update") {
		t.Errorf("error = %v, want it to name the command that installs the helper", err)
	}
}
