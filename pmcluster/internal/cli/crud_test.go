package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// newTestCLIEnv creates a throwaway data dir with a real store + config.yaml
// and points the package-level configPath at it, so the local-mode backend
// helpers (openStore → config.Load(configPath)) resolve against it. Returns
// the config and a restore func for the package globals.
func newTestCLIEnv(t *testing.T) (*config.Config, func()) {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		ListenAddr: "127.0.0.1:9090",
		DataDir:    dir,
		LogLevel:   "info",
	}
	body := fmt.Sprintf("listen_addr: %q\ndata_dir: %q\nlog_level: %q\n", cfg.ListenAddr, cfg.DataDir, cfg.LogLevel)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	// Creating the store runs the migrations — the DB must exist before
	// openStore refuses with 'data directory not initialised'.
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	origConfigPath := configPath
	configPath = cfgPath
	return cfg, func() { configPath = origConfigPath }
}

// newTestCmd returns a bare cobra command wired to the given RunE with the
// standard flags the real command group registers. Using a fresh command
// avoids mutating the package-level command vars across tests. The command
// gets a background context because cmd.Context() is nil before Execute.
func newTestCmd(use string, flags map[string]string, runE func(cmd *cobra.Command, args []string) error) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{Use: use, RunE: runE}
	cmd.SetContext(context.Background())
	for name, def := range flags {
		cmd.Flags().String(name, def, "")
	}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	return cmd, &out, &errOut
}

func TestConfigCRUD_RunPaths(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()

	// create (service scope)
	cmd, out, _ := newTestCmd("create <name>", map[string]string{"scope": "service", "stack": "demo", "kind": "env", "value": ""}, runConfigCreate)
	cmd.Flags().Set("scope", "service")
	cmd.Flags().Set("stack", "demo")
	cmd.Flags().Set("kind", "env")
	cmd.Flags().Set("value", "mode=prod")
	if err := cmd.RunE(cmd, []string{"demo_env"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(out.String(), `Config "demo_env" created`) {
		t.Fatalf("create output = %q", out.String())
	}

	// create (cluster scope, template)
	cmd2, _, _ := newTestCmd("create <name>", map[string]string{"scope": "cluster", "stack": "", "kind": "template", "value": ""}, runConfigCreate)
	cmd2.Flags().Set("scope", "cluster")
	cmd2.Flags().Set("kind", "template")
	cmd2.Flags().Set("value", "tpl-content")
	if err := cmd2.RunE(cmd2, []string{"my_template"}); err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	// duplicate create must fail with a helpful message
	cmdDup, _, _ := newTestCmd("create <name>", map[string]string{"scope": "service", "stack": "", "kind": "file", "value": ""}, runConfigCreate)
	cmdDup.Flags().Set("scope", "service")
	cmdDup.Flags().Set("value", "again")
	if err := cmdDup.RunE(cmdDup, []string{"demo_env"}); err == nil {
		t.Fatal("duplicate create: expected error")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate create error = %q, want 'already exists'", err)
	}

	// bad scope rejected
	cmdBad, _, _ := newTestCmd("create <name>", map[string]string{"scope": "cluster", "stack": "", "kind": "file", "value": ""}, runConfigCreate)
	cmdBad.Flags().Set("scope", "nope")
	cmdBad.Flags().Set("value", "x")
	if err := cmdBad.RunE(cmdBad, []string{"bad_scope"}); err == nil || !strings.Contains(err.Error(), "scope must be cluster or service") {
		t.Fatalf("bad scope error = %v", err)
	}

	// get returns content + metadata header
	cmdGet, outGet, _ := newTestCmd("get <name>", nil, runConfigGet)
	if err := cmdGet.RunE(cmdGet, []string{"demo_env"}); err != nil {
		t.Fatalf("get: %v", err)
	}
	got := outGet.String()
	if !strings.Contains(got, "# demo_env (scope: service") || !strings.Contains(got, "mode=prod") {
		t.Fatalf("get output = %q", got)
	}

	// get missing → friendly error
	cmdGetMiss, _, _ := newTestCmd("get <name>", nil, runConfigGet)
	if err := cmdGetMiss.RunE(cmdGetMiss, []string{"ghost"}); err == nil || !strings.Contains(err.Error(), `config "ghost" not found`) {
		t.Fatalf("get missing error = %v", err)
	}

	// list shows both rows
	cmdList, outList, _ := newTestCmd("list", nil, runConfigList)
	if err := cmdList.RunE(cmdList, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	ls := outList.String()
	if !strings.Contains(ls, "demo_env") || !strings.Contains(ls, "my_template") {
		t.Fatalf("list output = %q", ls)
	}

	// edit updates the value (hash changes)
	cmdEdit, outEdit, _ := newTestCmd("edit <name>", map[string]string{"value": ""}, runConfigEdit)
	cmdEdit.Flags().Set("value", "mode=staging")
	if err := cmdEdit.RunE(cmdEdit, []string{"demo_env"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(outEdit.String(), `Config "demo_env" updated`) {
		t.Fatalf("edit output = %q", outEdit.String())
	}
	cmdGet2, outGet2, _ := newTestCmd("get <name>", nil, runConfigGet)
	if err := cmdGet2.RunE(cmdGet2, []string{"demo_env"}); err != nil {
		t.Fatalf("get after edit: %v", err)
	}
	if !strings.Contains(outGet2.String(), "mode=staging") {
		t.Fatalf("get after edit output = %q", outGet2.String())
	}

	// rollback to previous version restores the old content
	cmdHist, outHist, _ := newTestCmd("history <name>", nil, runConfigHistory)
	if err := cmdHist.RunE(cmdHist, []string{"demo_env"}); err != nil {
		t.Fatalf("history: %v", err)
	}
	if !strings.Contains(outHist.String(), "ID") {
		t.Fatalf("history output = %q", outHist.String())
	}
	cmdRb, outRb, _ := newTestCmd("rollback <name>", nil, runConfigRollback)
	if err := cmdRb.RunE(cmdRb, []string{"demo_env"}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !strings.Contains(outRb.String(), `Config "demo_env" rolled back`) {
		t.Fatalf("rollback output = %q", outRb.String())
	}
	cmdGet3, outGet3, _ := newTestCmd("get <name>", nil, runConfigGet)
	if err := cmdGet3.RunE(cmdGet3, []string{"demo_env"}); err != nil {
		t.Fatalf("get after rollback: %v", err)
	}
	if !strings.Contains(outGet3.String(), "mode=prod") {
		t.Fatalf("get after rollback output = %q, want restored 'mode=prod'", outGet3.String())
	}

	// delete removes the row
	cmdDel, outDel, _ := newTestCmd("delete <name>", nil, runConfigDelete)
	if err := cmdDel.RunE(cmdDel, []string{"demo_env"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(outDel.String(), `Config "demo_env" deleted`) {
		t.Fatalf("delete output = %q", outDel.String())
	}
	cmdGet4, _, _ := newTestCmd("get <name>", nil, runConfigGet)
	if err := cmdGet4.RunE(cmdGet4, []string{"demo_env"}); err == nil {
		t.Fatal("get after delete: expected error")
	}
}

func TestSecretCRUD_RunPaths(t *testing.T) {
	cfg, restore := newTestCLIEnv(t)
	defer restore()

	// create with positional value (no docker daemon in tests → warn path, never fatal)
	cmd, out, _ := newTestCmd("create <name> [value]", map[string]string{"scope": "service", "stack": "demo"}, runSecretCreate)
	cmd.Flags().Set("scope", "service")
	cmd.Flags().Set("stack", "demo")
	if err := cmd.RunE(cmd, []string{"demo_db_pass", "s3cr3t"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(out.String(), `Secret "demo_db_pass" created`) || !strings.Contains(out.String(), "s3cr3t") {
		t.Fatalf("create output = %q", out.String())
	}

	// the encryption key was auto-created in the data dir
	if _, err := os.Stat(cfg.EncryptionKeyPath()); err != nil {
		t.Fatalf("encryption key not created: %v", err)
	}

	// duplicate create fails
	cmdDup, _, _ := newTestCmd("create <name> [value]", map[string]string{"scope": "service", "stack": ""}, runSecretCreate)
	cmdDup.Flags().Set("scope", "service")
	if err := cmdDup.RunE(cmdDup, []string{"demo_db_pass", "again"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate create error = %v", err)
	}

	// show reveals hash + metadata (never plaintext)
	cmdShow, outShow, _ := newTestCmd("show <name>", nil, runSecretShow)
	if err := cmdShow.RunE(cmdShow, []string{"demo_db_pass"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(outShow.String(), "sha256:") || strings.Contains(outShow.String(), "s3cr3t") {
		t.Fatalf("show output = %q (must never leak plaintext)", outShow.String())
	}

	// verify matches and mismatches
	cmdVok, outVok, _ := newTestCmd("verify <name> <value>", nil, runSecretVerify)
	if err := cmdVok.RunE(cmdVok, []string{"demo_db_pass", "s3cr3t"}); err != nil {
		t.Fatalf("verify ok: %v", err)
	}
	if !strings.Contains(outVok.String(), "hash matches") {
		t.Fatalf("verify ok output = %q", outVok.String())
	}
	cmdVbad, _, _ := newTestCmd("verify <name> <value>", nil, runSecretVerify)
	if err := cmdVbad.RunE(cmdVbad, []string{"demo_db_pass", "wrong"}); err == nil || !strings.Contains(err.Error(), "hash does not match") {
		t.Fatalf("verify bad error = %v", err)
	}

	// list shows the row (hash only)
	cmdList, outList, _ := newTestCmd("list", nil, runSecretList)
	if err := cmdList.RunE(cmdList, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(outList.String(), "demo_db_pass") {
		t.Fatalf("list output = %q", outList.String())
	}

	// delete removes the row
	cmdDel, _, _ := newTestCmd("delete <name>", nil, runSecretDelete)
	if err := cmdDel.RunE(cmdDel, []string{"demo_db_pass"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	cmdShow2, _, _ := newTestCmd("show <name>", nil, runSecretShow)
	if err := cmdShow2.RunE(cmdShow2, []string{"demo_db_pass"}); err == nil {
		t.Fatal("show after delete: expected error")
	}
}
