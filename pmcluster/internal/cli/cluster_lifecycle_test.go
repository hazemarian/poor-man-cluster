package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestClusterResetCmdRegistered verifies the `cluster reset` command is
// registered under `cluster` and exposes the --restore flag.
func TestClusterResetCmdRegistered(t *testing.T) {
	var found *cobra.Command
	for _, c := range clusterCmd.Commands() {
		if c.Name() == "reset" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("`reset` command not registered under `cluster`")
	}
	if found.Flags().Lookup("restore") == nil {
		t.Error("`reset` command missing --restore flag")
	}
}

// TestRunClusterDown_RequiresConfirmation verifies the cheap pre-docker run
// path: without --yes, `cluster down` refuses before touching Docker.
func TestRunClusterDown_RequiresConfirmation(t *testing.T) {
	cmd, _, _ := newTestCmd("down", nil, runClusterDown)
	cmd.Flags().Bool("yes", false, "")
	cmd.Flags().Bool("purge", false, "")
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("expected not-confirmed error, got %v", err)
	}
}

// TestRunClusterReset_RequiresInitialisedStore verifies the cheap pre-docker run
// path: with no data.db present, `cluster reset` errors before touching Docker.
func TestRunClusterReset_RequiresInitialisedStore(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("data_dir: "+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	orig := configPath
	configPath = cfgPath
	t.Cleanup(func() { configPath = orig })

	cmd, _, _ := newTestCmd("reset", map[string]string{"restore": ""}, runClusterReset)
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "not initialised") {
		t.Fatalf("expected not-initialised error, got %v", err)
	}
}
