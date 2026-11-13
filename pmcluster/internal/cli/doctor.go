package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/doctor"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
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
  github app         can the cluster mint an installation token, and does it grant what it needs
  github runners     are the self-hosted runners registered, running, and permitted
  last apply         did the last platform apply finish what it attempted
  restore drill      when a backup artifact was last really restored
  offsite            are this node's shipped artifacts still in the bucket
  disk               headroom on the volume root

On a node that is NOT the leader (a worker, or a follower manager) the same
command answers the NODE-LOCAL subset instead, because cluster state is only
current on the leader and this command will not read a stale copy:

  daemon             is this node's daemon listening (its /health)
  socket proxy       is the filtered docker socket present
  runner assignment  when this node last read its runner assignment, and from where
  local runner       this node's runner container, and the docker exposure it has
  disk               headroom on the volume root

Every cluster question it cannot answer is printed as a skipped line with the
reason, so the report is never mistaken for a clean bill of health.

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

	// A node that is demonstrably NOT the leader answers the node-local subset:
	// what this machine can measure, plus one skipped line per cluster question it
	// cannot. Refusing outright left an operator with no way to ask a WORKER why
	// its runner never started.
	if self, onFollower := followerSelf(cmd.Context()); onFollower {
		host, _ := os.Hostname()
		socketPath := cluster.DefaultGitHubRunnerSocketPath
		if p := envOr("PMCLUSTER_RUNNER_SOCKET", ""); p != "" {
			socketPath = p
		}
		// The mirror, if this node has one. openExistingStore never creates a database:
		// a node that was never initialised must keep saying so, and a worker simply has
		// nothing here (the check reports that rather than opening an empty one).
		var mirror *store.Store
		if s, err := openExistingStore(cfg.DBPath()); err == nil {
			mirror = s
			defer func() { _ = s.Close() }()
		}
		rep := doctor.RunNodeLocal(cmd.Context(), doctor.NodeInput{
			Hostname:   host,
			ListenAddr: cfg.ListenAddr,
			SocketPath: socketPath,
			StatePath:  agentStatePath(cfg.DataDir),
			VolumeRoot: envOr("PMCLUSTER_VOLUME_ROOT", "/var/stack/data"),
			Docker:     &runners.CLIDocker{},
			Store:      mirror,
			Role:       self.Role,
			// This machine's RAM, so the runner's cap is checked against the node
			// that would have to survive it. Unknown (0) skips the check.
			MemTotalMiB: hostMemTotalMiB(),
		})
		printReport(cmd, rep)
		if !rep.OK {
			return fmt.Errorf("doctor found failing checks on this node")
		}
		return nil
	}

	var st *store.Store
	if s, _, err := openStore(); err == nil {
		st = s
		defer func() { _ = s.Close() }()
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
	var runnerProbe doctor.RunnerProbe
	var app *githubapp.Local
	if st != nil && cipher != nil {
		app = newGitHubAppLocal(st, cipher, zerolog.Nop())
		gha = app
	}
	// The runner check reads settings, this node's runner container and GitHub's
	// org runner list. It needs the store for the settings and the docker CLI for
	// the container; without both it is skipped, not guessed.
	if st != nil && app != nil {
		host, _ := os.Hostname()
		runnerProbe = &runners.Manager{
			Store:  st,
			GitHub: app,
			Docker: &runners.CLIDocker{},
			Nodes:  dc,
			Node:   host,
			NodeMemory: func(ctx context.Context, node string) (int64, bool) {
				return runners.NodeMemoryMiB(ctx, st, node, time.Now())
			},
			Log: zerolog.Nop(),
		}
	}

	rep, err := doctor.Run(cmd.Context(), doctor.Input{
		Docker:         dc,
		Store:          st,
		Cipher:         cipher,
		GitHubApp:      gha,
		GitHubRunners:  runnerProbe,
		DataDir:        cfg.DataDir,
		Version:        buildinfo.Version,
		BackupStateDir: envOr("PMCLUSTER_BACKUP_STATE", "/var/lib/pmcluster-backup"),
		VolumeRoot:     volumeRoot,
	})
	if err != nil {
		return err
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
	} else {
		printReport(cmd, rep)
	}

	if !rep.OK {
		return fmt.Errorf("doctor found failing checks")
	}
	return nil
}

// printReport renders one report as the familiar check table.
func printReport(cmd *cobra.Command, rep *doctor.Report) {
	asJSON, _ := cmd.Flags().GetBool("json")
	if asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return
	}
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
		fmt.Fprintf(w, "%s %s	%s\n", mark, c.Name, c.Detail)
	}
	_ = w.Flush()
}

// followerSelf returns this node's swarm identity when it is a swarm member that is NOT
// the leader — the one case where this CLI answers from the local MIRROR instead of
// deferring to the leader (ADR 0003, slice 3).
//
// It is deliberately conservative: only a node that PROVES it is a member without
// leadership qualifies, so an unreachable Docker or a standalone box keeps the full answer
// it always had. Remote mode is excluded for the opposite reason: the user pointed the CLI
// at a daemon, and that daemon's own answer is the right one.
func followerSelf(ctx context.Context) (docker.SelfNode, bool) {
	if apiURL != "" {
		return docker.SelfNode{}, false
	}
	self, err := detectSelf(ctx)
	if err != nil {
		return docker.SelfNode{}, false
	}
	if !self.SwarmActive || self.IsLeader {
		return docker.SelfNode{}, false
	}
	return self, true
}
