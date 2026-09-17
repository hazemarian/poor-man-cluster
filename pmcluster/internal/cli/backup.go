package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backups"
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Trigger and inspect on-demand volume backups (offen)",
	Long: `pmcluster wraps offen/docker-volume-backup so you can trigger an
ad-hoc snapshot from the CLI or before a deploy. The actual archives
land at /var/backups/docker-volumes/ on the host that ran the backup;
this audit log records when each run started, finished, and which
files it produced.`,
}

var backupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Trigger an on-demand backup on the local node",
	Long: `Runs 'docker exec backup' against the local offen container and waits
for it to finish. Records the run in pmcluster's backups audit table
regardless of outcome (success or failure).`,
	RunE: runBackupCreate,
}

var backupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recorded backup runs (newest first)",
	RunE:  runBackupList,
}

func init() {
	backupCreateCmd.Flags().Duration("timeout", 5*time.Minute, "max time to wait for the backup to finish")
	backupListCmd.Flags().Int("limit", 20, "max rows to show (0 = all)")

	backupCmd.AddCommand(backupCreateCmd, backupListCmd)
	rootCmd.AddCommand(backupCmd)
}

func runBackupCreate(cmd *cobra.Command, _ []string) error {
	defer initCLITelemetry()()

	timeout, _ := cmd.Flags().GetDuration("timeout")

	svc, closeFn, err := backendBackups(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	id, paths, err := svc.Trigger(ctx, "", 0)
	if err != nil {
		return fmt.Errorf("backup failed (recorded as id=%d): %w", id, err)
	}
	printBackupResult(cmd, id, paths)
	return nil
}

func printBackupResult(cmd *cobra.Command, id int64, paths []string) {
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Backup id=%d succeeded.\n", id)
	if len(paths) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Archives:")
		for _, p := range paths {
			fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", p)
		}
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "(no archive paths parsed from offen output — see /var/backups/docker-volumes/ on the host)")
	}
}

func runBackupList(cmd *cobra.Command, _ []string) error {
	limit, _ := cmd.Flags().GetInt("limit")

	svc, closeFn, err := backendBackups(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	rows, err := svc.List(cmd.Context(), limit)
	if err != nil {
		return fmt.Errorf("list backups: %w", err)
	}
	return printBackups(cmd, rows)
}

func printBackups(cmd *cobra.Command, rows []backups.Run) error {
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no backups recorded yet)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tSTACK\tREVISION\tSTARTED\tFINISHED\tNOTES")
	for _, b := range rows {
		stack := "—"
		if b.StackName != "" {
			stack = b.StackName
		}
		rev := "—"
		if b.Revision != 0 {
			rev = fmt.Sprintf("%d", b.Revision)
		}
		finished := "—"
		if b.FinishedAt != 0 {
			finished = time.Unix(b.FinishedAt, 0).Format(time.RFC3339)
		}
		notes := b.ErrorMessage
		if notes == "" {
			notes = strings.Join(b.ArchivePaths, ",")
		}
		if len(notes) > 60 {
			notes = notes[:57] + "..."
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			b.ID, b.Status, stack, rev,
			time.Unix(b.StartedAt, 0).Format(time.RFC3339), finished, notes,
		)
	}
	return w.Flush()
}
