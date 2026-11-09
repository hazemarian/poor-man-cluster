package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/doctor"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check this platform's health: quorum, kit restore, secrets, drills, offsite, disk",
	Long: `Answers "is this platform healthy right now?" from measurements rather than
assumptions, one line per check:

  swarm              manager readiness, and whether quorum survives another loss
  version skew       which managers run a different pmcluster binary
  control-plane kit  does the newest Raft snapshot actually RESTORE — extracted
                     into a temp dir, opened, and its secrets decrypted
  secrets            does every stored secret decrypt with the current key
  last apply         did the last platform apply finish what it attempted
  restore drill      when a backup artifact was last really restored
  offsite            are this node's shipped artifacts still in the bucket
  disk               headroom on the volume root

Exit status is non-zero when any check fails, so it can gate a deploy or run
from cron. Nothing here writes: the kit drill extracts to a temporary
directory, the offsite check only lists, and the rest are reads.`,
	Args: cobra.NoArgs,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().Bool("json", false, "machine-readable report")
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	cfg := loadConfig(cmd)

	var dc runtime.Client
	if c, err := docker.New(); err == nil {
		dc = c
		defer func() { _ = c.Close() }()
	}

	var st *store.Store
	if s, _, err := openStore(); err == nil {
		st = s
		defer func() { _ = st.Close() }()
	}

	var cipher *credentials.Cipher
	if c, err := credentials.Open(cfg.EncryptionKeyPath()); err == nil {
		cipher = c
	}

	volumeRoot := envOr("PMCLUSTER_VOLUME_ROOT", "")
	if volumeRoot == "" && st != nil {
		volumeRoot = st.SettingDefault(cmd.Context(), cluster.SettingVolumeRoot(), "/var/stack/data")
	}

	// The GitHub App check needs both the store (encrypted registration) and
	// the cipher. Missing either skips the check with that reason instead of
	// reporting a healthy app nobody inspected.
	var gha doctor.GitHubAppProbe
	if st != nil && cipher != nil {
		gha = newGitHubAppLocal(st, cipher, zerolog.Nop())
	}

	rep, err := doctor.Run(cmd.Context(), doctor.Input{
		Docker:           dc,
		Store:            st,
		Cipher:           cipher,
		GitHubApp:        gha,
		CredentialHelper: credHelperProbe{},
		DataDir:          cfg.DataDir,
		Version:          buildinfo.Version,
		BackupStateDir:   envOr("PMCLUSTER_BACKUP_STATE", "/var/lib/pmcluster-backup"),
		VolumeRoot:       volumeRoot,
	})
	if err != nil {
		return err
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
	} else {
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		for _, c := range rep.Checks {
			mark := "✓"
			switch c.Status {
			case doctor.StatusFail:
				mark = "✖"
			case doctor.StatusWarn:
				mark = "⚠"
			case doctor.StatusSkip:
				mark = "·"
			}
			fmt.Fprintf(w, "%s %s\t%s\n", mark, c.Name, c.Detail)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	if !rep.OK {
		return fmt.Errorf("doctor found failing checks")
	}
	return nil
}
