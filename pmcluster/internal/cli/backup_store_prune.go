package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

var backupStorePruneCmd = &cobra.Command{
	Use:   "prune-store",
	Short: "Apply a shorter retention to the in-cluster store only (the bucket keeps its history)",
	Long: `The store and the offsite bucket share one retention policy, but they are not
the same kind of copy: the bucket is the archive, the store is the copy that
lives on a node's own disk. On a small node that asymmetry is the difference
between 6 GB free and 40 GB used.

This applies --keep-hours to the STORE leg only, right now, without waiting for
the hourly run: every artifact older than the window goes, the newest --keep-last
per node and stack stays, and the control-plane family is trimmed to two. The
offsite bucket is not touched.`,
	Args: cobra.NoArgs,
	RunE: runPruneStore,
}

func init() {
	backupStorePruneCmd.Flags().Int("keep-hours", 24, "keep every artifact newer than this on the store")
	backupStorePruneCmd.Flags().Int("keep-last", 2, "always keep the newest N artifacts per node and stack")
	backupStorePruneCmd.Flags().Bool("dry-run", false, "report what would be deleted")
}

func runPruneStore(cmd *cobra.Command, _ []string) error {
	hours, _ := cmd.Flags().GetInt("keep-hours")
	keepLast, _ := cmd.Flags().GetInt("keep-last")
	dry, _ := cmd.Flags().GetBool("dry-run")
	ctx := cmd.Context()

	var store *shipTarget
	for _, t := range backupShipTargets() {
		if t.Label == "store" {
			tt := t
			store = &tt
			break
		}
	}
	if store == nil {
		return fmt.Errorf("this node has no in-cluster store target configured")
	}

	policy := backups.Retention{KeepHours: hours, KeepLast: keepLast}
	res, err := backups.PruneS3(ctx, store.S3, policy, time.Now(), dry)
	if err != nil {
		return fmt.Errorf("prune store: %w", err)
	}
	cps, cpkept, err := backups.PruneControlPlaneArchives(ctx, store.S3, time.Now(), dry)
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "control-plane retention skipped: %v\n", err)
	}
	verb := "deleted"
	if dry {
		verb = "would delete"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s:%s — %s %d artifact(s) older than %dh, kept %d; control-plane %s %d, kept %d\n",
		store.Label, store.S3.Bucket, verb, len(res.Deleted), hours, res.Kept, verb, cps, cpkept)
	if len(res.Failed) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "%d object(s) could not be deleted\n", len(res.Failed))
	}
	return nil
}
