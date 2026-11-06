package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/controlplane"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
)

var clusterKitCmd = &cobra.Command{
	Use:   "kit",
	Short: "Inspect the control-plane survivor kit (the Raft-replicated data.db + key)",
}

var clusterKitVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Restore the newest control-plane snapshot into a temp dir and prove it works",
	Long: `The control plane survives losing its leader through a Raft-replicated survivor
kit: pmcluster_state_<ts> (data.db + config/) and pmcluster_state_key_<ts> (.encryption_key),
published by the leader every five minutes. That pair is only ever exercised
when a leader actually dies — the worst moment to find out it does not restore.

This restores the newest pair into a temporary directory and checks, without
touching the live data dir:

  - both configs exist and the tar extracts (a missing key config is a hard
    failure: a ciphertext without its key is not a backup);
  - the daemon's own store opens the restored data.db and its schema is current;
  - the key stored beside the ciphertext decrypts the secrets stored with it.

Run it on any manager, and on a schedule: a kit that has silently rotted looks
exactly like a healthy one until the leader is gone.`,
	Args: cobra.NoArgs,
	RunE: runClusterKitVerify,
}

func init() {
	clusterKitVerifyCmd.Flags().Bool("json", false, "machine-readable report")
	clusterKitCmd.AddCommand(clusterKitVerifyCmd)
}

func runClusterKitVerify(cmd *cobra.Command, _ []string) error {
	cfg := loadConfig(cmd)
	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dc.Close() }()

	kit := &controlplane.Kit{Docker: dc, DataDir: dataDir(cfg)}
	rep, err := kit.Verify(cmd.Context())
	if err != nil {
		return err
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
	} else {
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "snapshot\t%s (%s old)\n", rep.SnapshotAt.UTC().Format("2006-01-02T15:04:05Z"), rep.SnapshotAge)
		fmt.Fprintf(w, "state config\t%s\n", rep.StateConfig)
		fmt.Fprintf(w, "key config\t%s (key %s…)\n", rep.KeyConfig, rep.KeyFingerprint)
		fmt.Fprintf(w, "extracted\t%d file(s)\n", rep.Files)
		fmt.Fprintf(w, "database\t%d table(s), schema %s (%d migration(s))\n", rep.Tables, rep.SchemaVersion, rep.Migrations)
		fmt.Fprintf(w, "content\t%d setting(s), %d config(s), %d secret(s)\n", rep.Settings, rep.Configs, rep.Secrets)
		if rep.Secrets > 0 {
			fmt.Fprintf(w, "decryption\t%d/%d secret(s) decrypt with the snapshot's own key\n", rep.SecretsOK, rep.Secrets)
		} else {
			fmt.Fprintf(w, "decryption\tno secrets stored, nothing to probe\n")
		}
		for _, warn := range rep.Warnings {
			fmt.Fprintf(w, "warning\t⚠ %s\n", warn)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	if len(rep.Warnings) > 0 {
		return fmt.Errorf("control-plane kit verify reported %d problem(s) — this snapshot would promote a leader that cannot read part of its own state", len(rep.Warnings))
	}
	return nil
}

// dataDir resolves the local data dir the same way the daemon does, so the
// drill reads the same config the running process uses.
func dataDir(cfg *config.Config) string {
	if cfg != nil {
		return cfg.DataDir
	}
	return ""
}
