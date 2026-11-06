package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodever"
)

var nodeVersionsCmd = &cobra.Command{
	Use:   "versions",
	Short: "Show the pmcluster binary version each manager reports, and any skew",
	Long: `Every manager publishes its binary version into the swarm every ten minutes
(pmcluster_nodever_<host> configs). This is the fleet view: a node two releases
behind used to be invisible until the next upgrade script printed its "before:"
line — which is how one node sat two releases back unnoticed.

Workers cannot create configs, so they cannot report. That is a platform limit,
not a gap in this command, and it is stated in the output rather than hidden.`,
	Args: cobra.NoArgs,
	RunE: runNodeVersions,
}

func runNodeVersions(cmd *cobra.Command, _ []string) error {
	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dc.Close() }()

	skew, err := nodever.Read(cmd.Context(), dc, buildinfo.Version)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(skew.Nodes) == 0 {
		fmt.Fprintln(out, "no manager has published a version yet (they publish on start and every 10 minutes)")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOSTNAME\tVERSION\tSTATUS")
	for _, n := range skew.Nodes {
		status := "ok"
		if skew.Reference != "" && n.Version != skew.Reference {
			status = "SKEWED (this node runs " + skew.Reference + ")"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", n.Hostname, n.Version, status)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(skew.Stale) > 0 {
		fmt.Fprintf(out, "\n%d node(s) are not on %s. Upgrade them with hack/upgrade-nodes.sh.\n",
			len(skew.Stale), skew.Reference)
	}
	fmt.Fprintln(out, "workers do not report: a worker cannot create swarm configs.")
	return nil
}

func init() {
	nodeCmd.AddCommand(nodeVersionsCmd)
}
