package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// TestStackAckCommandRegistered pins `pmcluster stack ack` to the command
// group: name, arity, and help text that tells the operator what
// acknowledging a storage failover actually does (the subcommand silently
// missing would strand the failover badge with no way to clear it).
func TestStackAckCommandRegistered(t *testing.T) {
	var ack *cobra.Command
	for _, c := range stackCmd.Commands() {
		if c.Name() == "ack" {
			ack = c
			break
		}
	}
	if ack == nil {
		t.Fatal("stack ack command not registered under stackCmd")
		return
	}
	if ack.Use != "ack <stack-name>" {
		t.Errorf("ack Use = %q, want %q", ack.Use, "ack <stack-name>")
	}
	if ack.Args == nil {
		t.Error("ack command must declare an Args validator (exactly one stack name)")
	}
	if !strings.Contains(strings.ToLower(ack.Short), "acknowledge") {
		t.Errorf("ack Short = %q, want it to mention 'acknowledge'", ack.Short)
	}
	// The Long text and the rendered help use the gerund ("Acknowledging"),
	// so match the shared stem rather than the exact word.
	if !strings.Contains(strings.ToLower(ack.Long), "acknowledg") {
		t.Errorf("ack Long = %q, want it to mention 'acknowledg(e/ing/ed)'", ack.Long)
	}

	// Help output must render and carry the acknowledge wording.
	var buf bytes.Buffer
	ack.SetOut(&buf)
	ack.SetErr(&buf)
	t.Cleanup(func() {
		ack.SetOut(nil)
		ack.SetErr(nil)
	})
	if err := ack.Help(); err != nil {
		t.Fatalf("ack help: %v", err)
	}
	help := strings.ToLower(buf.String())
	if !strings.Contains(help, "acknowledg") {
		t.Errorf("ack help missing 'acknowledg...' wording:\n%s", buf.String())
	}
	if !strings.Contains(help, "ack <stack-name>") {
		t.Errorf("ack help missing usage line:\n%s", buf.String())
	}
}

// TestStackAckRunPath drives the real `stack ack` run function against a
// local store: acknowledging an open marker succeeds and prints the
// confirmation, while acknowledging a stack with no marker fails with the
// store's not-found sentinel (surfaced by the API as 404 in daemon mode).
func TestStackAckRunPath(t *testing.T) {
	cfg, restore := newTestCLIEnv(t)
	defer restore()

	// Keep the local backend selected even if another test toggled remote
	// mode (apiURL is package-global).
	prevURL := apiURL
	apiURL = ""
	t.Cleanup(func() { apiURL = prevURL })

	// Seed an open (unacknowledged) failover marker in the test store.
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SetStackFailover(context.Background(), store.StackFailover{
		StackName: "demo", FromNode: "node-a", ToNode: "node-b", At: 1700000001,
	}); err != nil {
		_ = st.Close()
		t.Fatalf("SetStackFailover: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	cmd, out, _ := newTestCmd("ack <stack-name>", nil, runStackAck)
	if err := cmd.RunE(cmd, []string{"demo"}); err != nil {
		t.Fatalf("stack ack demo: %v", err)
	}
	if !strings.Contains(out.String(), "Acknowledged the failover for demo") {
		t.Errorf("ack output = %q, want the acknowledgement confirmation", out.String())
	}

	// The marker is now acknowledged on disk.
	st2, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st2.Close() }()
	fo, err := st2.StackFailover(context.Background(), "demo")
	if err != nil {
		t.Fatalf("StackFailover: %v", err)
	}
	if !fo.Acked {
		t.Error("marker not acknowledged after `stack ack demo`")
	}

	// A stack with no marker fails with the not-found sentinel.
	cmdGhost, _, _ := newTestCmd("ack <stack-name>", nil, runStackAck)
	err = cmdGhost.RunE(cmdGhost, []string{"ghost"})
	if err == nil {
		t.Fatal("stack ack ghost: expected an error (no open marker)")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("ack ghost error = %v, want the not-found sentinel", err)
	}
}
