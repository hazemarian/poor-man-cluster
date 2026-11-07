package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

var backupStorePruneCmd = &cobra.Command{
	Use:   "prune-remote",
	Short: "Apply retention to the remote legs now (store, offsite bucket, or both)",
	Long: `The store and the offsite bucket share one retention policy, but they are not
the same kind of copy: the bucket is the archive, the store is the copy that
lives on a node's own disk. On a small node that asymmetry is the difference
between 6 GB free and 40 GB used.

This applies the policy to a remote leg right now, without waiting for the hourly
run. --legacy also ages out the whole-node archives written by the pre-cutover
agent (names without a -stack- segment): they are invisible to normal retention
on purpose, so on a fleet that has moved they sit in every bucket forever.`,
	Args: cobra.NoArgs,
	RunE: runPruneRemote,
}

func init() {
	backupStorePruneCmd.Flags().String("target", "store", "which leg: store, s3 (offsite), or all")
	backupStorePruneCmd.Flags().Int("keep-hours", 24, "keep every artifact newer than this")
	backupStorePruneCmd.Flags().Int("keep-days", 3, "sparse window: one artifact per day up to this age")
	backupStorePruneCmd.Flags().Int("keep-last", 2, "always keep the newest N artifacts per node and stack")
	backupStorePruneCmd.Flags().Bool("legacy", false, "also prune whole-node archives from before the per-stack agent")
	backupStorePruneCmd.Flags().Bool("dry-run", false, "report what would be deleted")
}

func runPruneRemote(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	flags := cmd.Flags()
	hours, _ := flags.GetInt("keep-hours")
	keepDays, _ := flags.GetInt("keep-days")
	keepLast, _ := flags.GetInt("keep-last")
	target, _ := flags.GetString("target")
	legacy, _ := flags.GetBool("legacy")
	dry, _ := flags.GetBool("dry-run")

	var legs []shipTarget
	for _, t := range backupShipTargets() {
		if target == "all" || target == "" || t.Label == target {
			legs = append(legs, t)
		}
	}
	if len(legs) == 0 {
		return fmt.Errorf("this node has no %q target configured", target)
	}

	policy := backups.Retention{KeepHours: hours, KeepDays: keepDays, KeepLast: keepLast, IncludeLegacy: legacy}
	verb := "deleted"
	if dry {
		verb = "would delete"
	}
	out := cmd.OutOrStdout()
	for _, leg := range legs {
		res, err := backups.PruneS3(ctx, leg.S3, policy, time.Now(), dry)
		if err != nil {
			fmt.Fprintf(out, "%s:%s — ERROR %v\n", leg.Label, leg.S3.Bucket, err)
			continue
		}
		fmt.Fprintf(out, "%s:%s — %s %d artifact(s) (kept %d, legacy-skipped %d)\n",
			leg.Label, leg.S3.Bucket, verb, len(res.Deleted), res.Kept, res.SkippedOld)
		if len(res.Failed) > 0 {
			fmt.Fprintf(out, "  %d object(s) could not be deleted\n", len(res.Failed))
		}
		// The control-plane family has its own producer and its own age-out.
		if del, kept, cerr := backups.PruneControlPlaneArchives(ctx, leg.S3, time.Now(), dry); cerr != nil {
			fmt.Fprintf(out, "%s:%s — control-plane retention skipped: %v\n", leg.Label, leg.S3.Bucket, cerr)
		} else if del > 0 {
			fmt.Fprintf(out, "%s:%s — %s %d control-plane archive(s), kept %d\n", leg.Label, leg.S3.Bucket, verb, del, kept)
		}
	}
	return nil
}
