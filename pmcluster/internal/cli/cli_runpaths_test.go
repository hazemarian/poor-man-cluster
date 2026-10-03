package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestWebhookCRUD_RunPaths drives the store-backed webhook commands: add,
// list, deliveries, duplicate add, remove, missing-source errors.
func TestWebhookCRUD_RunPaths(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()

	// add
	cmd, out, _ := newTestCmd("add <source>", map[string]string{"description": ""}, runWebhookAdd)
	cmd.Flags().Set("description", "ci deploys")
	if err := cmd.RunE(cmd, []string{"deploy"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(out.String(), `Webhook source "deploy" created`) {
		t.Fatalf("add output = %q", out.String())
	}
	if !strings.Contains(out.String(), "sha256=<hmac-sha256") {
		t.Fatalf("add output missing secret instructions: %q", out.String())
	}

	// duplicate add fails
	cmdDup, _, _ := newTestCmd("add <source>", map[string]string{"description": ""}, runWebhookAdd)
	if err := cmdDup.RunE(cmdDup, []string{"deploy"}); err == nil || !strings.Contains(err.Error(), `already exists`) {
		t.Fatalf("duplicate add error = %v", err)
	}

	// list shows the source
	cmdList, outList, _ := newTestCmd("list", nil, runWebhookList)
	if err := cmdList.RunE(cmdList, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(outList.String(), "deploy") || !strings.Contains(outList.String(), "ci deploys") {
		t.Fatalf("list output = %q", outList.String())
	}

	// deliveries on a source with none recorded
	cmdDel, outDel, _ := newTestCmd("deliveries <source>", map[string]string{"limit": "10"}, runWebhookDeliveries)
	cmdDel.Flags().Set("limit", "10")
	if err := cmdDel.RunE(cmdDel, []string{"deploy"}); err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	if !strings.Contains(outDel.String(), "no deliveries recorded") {
		t.Fatalf("deliveries output = %q", outDel.String())
	}

	// remove
	cmdRm, outRm, _ := newTestCmd("remove <source>", nil, runWebhookRemove)
	if err := cmdRm.RunE(cmdRm, []string{"deploy"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(outRm.String(), `Webhook source "deploy" removed`) {
		t.Fatalf("remove output = %q", outRm.String())
	}

	// removing a missing source fails helpfully
	cmdRm2, _, _ := newTestCmd("remove <source>", nil, runWebhookRemove)
	if err := cmdRm2.RunE(cmdRm2, []string{"ghost"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("remove missing error = %v", err)
	}
}

// TestSettingsUsage_RunPaths drives the cluster settings get/set/list paths and
// the usage report against a local store.
func TestSettingsUsage_RunPaths(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()

	// list (empty store → headers + em-dash values)
	cmd, out, _ := newTestCmd("settings", nil, runClusterSettings)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("settings list: %v", err)
	}
	if !strings.Contains(out.String(), "KEY") {
		t.Fatalf("settings list output = %q", out.String())
	}

	// set a value
	cmdSet, outSet, _ := newTestCmd("settings set", nil, runClusterSettings)
	if err := cmdSet.RunE(cmdSet, []string{"set", "volume_root=/data"}); err != nil {
		t.Fatalf("settings set: %v", err)
	}
	if !strings.Contains(outSet.String(), "Settings updated") {
		t.Fatalf("settings set output = %q", outSet.String())
	}

	// get the value back
	cmdGet, outGet, _ := newTestCmd("settings get", nil, runClusterSettings)
	if err := cmdGet.RunE(cmdGet, []string{"get", "volume_root"}); err != nil {
		t.Fatalf("settings get: %v", err)
	}
	if !strings.Contains(outGet.String(), "volume_root=/data") {
		t.Fatalf("settings get output = %q", outGet.String())
	}

	// get unknown key fails
	cmdGetBad, _, _ := newTestCmd("settings get", nil, runClusterSettings)
	if err := cmdGetBad.RunE(cmdGetBad, []string{"get", "nope"}); err == nil || !strings.Contains(err.Error(), `unknown setting`) {
		t.Fatalf("settings get missing error = %v", err)
	}

	// set without key=value fails
	cmdSetBad, _, _ := newTestCmd("settings set", nil, runClusterSettings)
	if err := cmdSetBad.RunE(cmdSetBad, []string{"set", "novalue"}); err == nil || !strings.Contains(err.Error(), "key=value") {
		t.Fatalf("settings set bad error = %v", err)
	}

	// usage: no references in an empty store
	cmdUsage, outUsage, _ := newTestCmd("usage", nil, runUsage)
	if err := cmdUsage.RunE(cmdUsage, nil); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if !strings.Contains(outUsage.String(), "no config references found") || !strings.Contains(outUsage.String(), "no secret references found") {
		t.Fatalf("usage output = %q", outUsage.String())
	}
}

// TestSettingsMaskSecrets checks that printSettings masks any key containing
// "secret".
func TestSettingsMaskSecrets(t *testing.T) {
	settings := map[string]string{"domain": "example.test", "backup_s3_secret_key": "hunter2"}
	cmd, out, _ := newTestCmd("settings", nil, func(cmd *cobra.Command, _ []string) error {
		return printSettings(cmd, settings)
	})
	if err := printSettings(cmd, settings); err != nil {
		t.Fatalf("printSettings: %v", err)
	}
	s := out.String()
	if strings.Contains(s, "hunter2") {
		t.Fatalf("printSettings leaked secret: %q", s)
	}
	if !strings.Contains(s, "********") {
		t.Fatalf("printSettings did not mask secret key: %q", s)
	}
	if !strings.Contains(s, "example.test") {
		t.Fatalf("printSettings dropped non-secret key: %q", s)
	}
}

// TestUserListRemove drives the store-backed user list/remove commands.
func TestUserListRemove(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()

	// list (empty → no rows)
	cmdList, outList, _ := newTestCmd("user list", nil, runUserList)
	if err := cmdList.RunE(cmdList, nil); err != nil {
		t.Fatalf("user list: %v", err)
	}
	if !strings.Contains(outList.String(), "no API users") {
		t.Fatalf("user list output = %q", outList.String())
	}

	// remove a missing key fails helpfully
	cmdRm, _, _ := newTestCmd("user remove <key>", nil, runUserRemove)
	if err := cmdRm.RunE(cmdRm, []string{"nope"}); err == nil {
		t.Fatal("user remove missing: expected error")
	}
}
