package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/settings"
)

// `pmcluster cluster settings` printed the offsite S3 secret and the SSO client
// secret in cleartext while docs/settings.md promised "secret-typed keys masked".
// The doc is the contract; stdout is the leak path (shell history, scrollback, a
// pasted transcript). These two tests pin both halves of it.
func TestSettingsPrintMasksSecretValuesByDefault(t *testing.T) {
	s := settings.Settings{
		"backup_s3_secret_key": "offsite-secret-value",
		"backup_s3_access_key": "ACCESS-KEY-VALUE",
		"sso_client_secret":    "sso-secret-value",
		"domain":               "example.com",
		"volume_root":          "",
	}
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := printSettings(cmd, s, false); err != nil {
		t.Fatalf("printSettings: %v", err)
	}
	out := buf.String()

	for _, secret := range []string{"offsite-secret-value", "ACCESS-KEY-VALUE", "sso-secret-value"} {
		if strings.Contains(out, secret) {
			t.Errorf("a secret reached stdout unmasked (%q):\n%s", secret, out)
		}
	}
	if !strings.Contains(out, settings.Mask) {
		t.Errorf("expected masked values (%q) in output:\n%s", settings.Mask, out)
	}
	if !strings.Contains(out, "example.com") {
		t.Errorf("a non-secret value was masked too — that hides the answer the operator needs:\n%s", out)
	}
	// The operator has to be told the value exists and how to see it, or masking
	// reads as "unset".
	if !strings.Contains(out, "--show-secrets") {
		t.Errorf("output masks secrets without saying so or how to unmask:\n%s", out)
	}
}

func TestSettingsPrintShowsSecretsOnlyWhenAsked(t *testing.T) {
	s := settings.Settings{"backup_s3_secret_key": "offsite-secret-value"}
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := printSettings(cmd, s, true); err != nil {
		t.Fatalf("printSettings: %v", err)
	}
	if !strings.Contains(buf.String(), "offsite-secret-value") {
		t.Errorf("--show-secrets must print the value; got:\n%s", buf.String())
	}
}
