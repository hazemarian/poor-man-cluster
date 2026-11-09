package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
)

var clusterLeaderCmd = &cobra.Command{
	Use:   "leader <node>",
	Short: "Move Swarm leadership onto a manager (for testing failover)",
	Long: `Forces the Raft election onto <node> by demoting the current leader for the
duration of the election and promoting it back afterwards. Swarm has no
leadership-transfer API, so this is the only supported way to move leadership
without taking a node down: the survivor set elects a new leader, and the old
one rejoins as a plain manager.

This is a TESTING affordance — it is how failover behaviour (control-plane kit
adoption, the collector pin, the storage set) is exercised on a live cluster
without killing a node. Nothing automatic calls it.

Storage membership is NOT what this command is for. ` + "`storage_nodes`" + ` changes only in one
direction, as everywhere else: if the node you promote becomes leader and is missing from the
set, the next apply adds it (the leader invariant). Nothing is ever removed because it lost
leadership, and the nodes that were storage before the move still are.

Refused when <node> is not a manager, when forcing the election would leave
fewer than three managers, and when the swarm has no quorum.`,
	Args: cobra.ExactArgs(1),
	RunE: runClusterLeader,
}

func init() {
	clusterCmd.AddCommand(clusterLeaderCmd)
}

// clusterLeaderMoveOptions is the election timing `cluster leader` uses.
// Production takes the defaults; tests shorten the window.
var clusterLeaderMoveOptions = cluster.LeaderMoveOptions{}

func runClusterLeader(cmd *cobra.Command, args []string) error {
	target := args[0]
	out := cmd.OutOrStdout()

	var (
		res cluster.LeaderMove
		err error
	)
	if rc := remoteClient(cmd); rc != nil {
		res, err = rc.MoveLeader(cmd.Context(), target)
	} else {
		dc, derr := openDocker()
		if derr != nil {
			return derr
		}
		defer func() { _ = dc.Close() }()
		res, err = cluster.MoveLeadership(cmd.Context(), dc, target, clusterLeaderMoveOptions)
	}
	if err != nil {
		return err
	}

	if !res.Changed() {
		fmt.Fprintf(out, "%s is already the swarm leader — nothing to move\n", res.Leader)
		return nil
	}
	fmt.Fprintf(out, "✓ leadership moved to %s (from %s, %d forced election(s))\n", res.Leader, res.From, res.Elections)
	fmt.Fprintln(out, "  storage_nodes was not touched: storage membership is the operator's decision, not a consequence of leadership.")
	return nil
}
