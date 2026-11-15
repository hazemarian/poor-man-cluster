package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/clustersnapshot"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
)

var clusterMirrorCmd = &cobra.Command{
	Use:   "mirror",
	Short: "Inspect the cluster state this node mirrors (the mTLS pull)",
}

var clusterMirrorVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Prove a promotion would work from the cluster state this node holds",
	Long: `Cluster state reaches this node over the swarm-mTLS pull and is mirrored into its
own store: every table the control plane lives in, minus the ones that describe the
node they were written on. Adoption copies nothing (ADR 0005), so the store a node
mirrors is exactly what it would serve as leader.

That makes THIS the drill the control-plane kit verify used to be, and it answers the
same question from the state a promotion would actually use:

  - which leader's version is applied here, and when this node last got a successful
    answer from it (a 304 counts: the steady state of a healthy mirror is "nothing
    changed", not "nothing happened");
  - how many rows every mirrored table carries — the stack list, its revisions, the
    configs, the users — because a version number says nothing about what came with it;
  - whether this node holds the encryption key that decrypts the secrets it mirrors.

A stale mirror is reported as a warning, not a failure: a promotion on it is accepted
and the adoption logs the age. No snapshot at all, or no key, is a failure — those
would promote a leader that cannot serve its own cluster.

Run it on any node, and on a schedule: a mirror that has silently rotted looks exactly
like a healthy one until the leader is gone.`,
	Args: cobra.NoArgs,
	RunE: runClusterMirrorVerify,
}

func init() {
	clusterMirrorVerifyCmd.Flags().Bool("json", false, "machine-readable report")
	clusterMirrorCmd.AddCommand(clusterMirrorVerifyCmd)
}

func runClusterMirrorVerify(cmd *cobra.Command, _ []string) error {
	cfg := loadConfig(cmd)
	st, err := openExistingStore(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open the local store: %w (a worker has none; a manager mirrors into this file)", err)
	}
	defer func() { _ = st.Close() }()

	// The key is not part of the mirror, so it is asked for here: a node holding the
	// cluster's ciphertext but not the key that opens it would promote a leader that 500s
	// on every secret read.
	cipher, cipherErr := credentials.Open(cfg.EncryptionKeyPath())
	hasKey := cipherErr == nil && cipher != nil

	rep, err := clustersnapshot.Verify(cmd.Context(), st, hasKey, time.Now())
	if err != nil {
		return err
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
	} else {
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "source\t%s\n", rep.Source)
		if !rep.NeverApplied {
			fmt.Fprintf(w, "version\t%d\n", rep.Version)
			fmt.Fprintf(w, "applied\t%s\n", time.Unix(rep.AppliedAt, 0).UTC().Format(time.RFC3339))
			fmt.Fprintf(w, "confirmed\t%s ago\n", (time.Duration(rep.AgeSeconds) * time.Second).String())
		} else {
			fmt.Fprintf(w, "version\t%d (this node's own)\n", rep.Version)
		}
		fmt.Fprintf(w, "key\t%s\n", presentOrMissing(rep.KeyPresent))
		fmt.Fprintf(w, "rows\t%d in %d table(s)\n", rep.TotalRows, len(rep.Tables))
		if err := w.Flush(); err != nil {
			return err
		}
		w = tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		for _, table := range sortedKeys(rep.Tables) {
			fmt.Fprintf(w, "  %s\t%d\n", table, rep.Tables[table])
		}
		for _, warn := range rep.Warnings {
			fmt.Fprintf(w, "warning\t⚠ %s\n", warn)
		}
		for _, problem := range rep.Problems {
			fmt.Fprintf(w, "problem\t✗ %s\n", problem)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	if !rep.OK() {
		return fmt.Errorf("cluster mirror verify found %d problem(s) — a promotion from this node would serve cluster state it cannot use", len(rep.Problems))
	}
	return nil
}

func presentOrMissing(present bool) string {
	if present {
		return "present"
	}
	return "MISSING"
}

// sortedKeys keeps the table list in a stable order, so two runs can be diffed by eye.
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
