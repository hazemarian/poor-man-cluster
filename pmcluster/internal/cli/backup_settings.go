package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
)

// Backup on/off and the schedule, set from the CLI.
//
// The schedule these write is the same one the console edits and the same one
// the node agent reads before every run. Nothing here touches the systemd timer:
// the timer only asks "is it time?".
var backupEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Turn the backup pipeline on (setting: backup_enabled)",
	Long: `Turns backups on. The next time a node's timer fires, its agent will ask the
control plane whether the scheduled minute has arrived and run if it has.

This does not install the agent on a node that never had one: use
'pmcluster backup-agent install' on the node for that.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return setBackupSetting(cmd, cluster.SettingBackupEnabled(), "true")
	},
}

var backupDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Turn the backup pipeline off (setting: backup_enabled)",
	Long: `Turns backups off. Agents keep firing on their hourly heartbeat and exit
immediately without freezing anything, which is deliberate: the switch is a
setting, and a node with no timer could never be turned back on remotely.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return setBackupSetting(cmd, cluster.SettingBackupEnabled(), "false")
	},
}

var backupScheduleCmd = &cobra.Command{
	Use:   "schedule [cron]",
	Short: "Show or set the backup schedule (5-field cron)",
	Long: `With no argument, prints the schedule in force. With one, sets it.

Five fields: minute hour day-of-month month day-of-week. Only '*', a number and
comma lists are accepted — '0 * * * *' (hourly), '0 2 * * *' (02:00 daily),
'30 1,13 * * *' (twice a day). Steps and ranges are refused rather than guessed
at, because a schedule this platform cannot evaluate exactly would become a
silent gap in the backup history.

The schedule lives in the cluster settings. Do not edit the pmcluster-backup
timer: it is only a heartbeat.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			st, _, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			expr := st.SettingDefault(cmd.Context(), cluster.SettingBackupCron(), cluster.DefaultBackupCron)
			fmt.Fprintf(cmd.OutOrStdout(), "schedule: %s\n", expr)
			return nil
		}
		expr := strings.TrimSpace(args[0])
		// Validate against a real instant so a typo fails here, not at 03:00.
		if _, err := cluster.CronDue(expr, time.Now()); err != nil {
			return err
		}
		return setBackupSetting(cmd, cluster.SettingBackupCron(), expr)
	},
}

func setBackupSetting(cmd *cobra.Command, key, value string) error {
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if err := st.SetSetting(cmd.Context(), key, value); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s = %s\n", key, value)
	return nil
}
