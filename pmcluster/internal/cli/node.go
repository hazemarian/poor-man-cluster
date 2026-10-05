package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Inspect Swarm nodes and fetch join tokens",
}

var nodeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Swarm nodes (id, role, availability, status, leader)",
	RunE:  runNodeList,
}

var nodeJoinTokenCmd = &cobra.Command{
	Use:   "join-token [worker|manager]",
	Short: "Print the swarm join token for the given role",
	Long: `Prints the literal command to run on a new node:

  docker swarm join --token <TOKEN> <MANAGER>:2377

Tokens are sensitive — they grant Swarm membership.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runNodeJoinToken,
}

var nodePromoteCmd = &cobra.Command{
	Use:   "promote <hostname>",
	Short: "Make a node a storage node (pmcluster.storage label + storage_nodes setting)",
	Long: `Registers <hostname> as a storage node: adds it to the storage_nodes
cluster setting AND stamps the pmcluster.storage=true Swarm node label.

Stateful app stacks (volume-holding services without an explicit placement)
are then allowed to round-robin onto this node, and the backup agent runs a
global task on every storage node.

Works for workers too — storage nodes are role-agnostic.`,
	Args: cobra.ExactArgs(1),
	RunE: runNodePromote,
}

var nodeDemoteCmd = &cobra.Command{
	Use:   "demote <hostname>",
	Short: "Remove a node as a storage node (label + storage_nodes setting)",
	Long: `Removes <hostname> from the storage_nodes cluster setting and clears
the pmcluster.storage node label. Stateful stacks currently pinned to this
node are NOT moved automatically — run 'pmcluster stack move <stack> --to
<node>' for each before demoting, or accept that they stay put.`,
	Args: cobra.ExactArgs(1),
	RunE: runNodeDemote,
}

func init() {
	nodeCmd.AddCommand(nodeListCmd, nodeJoinTokenCmd, nodePromoteCmd, nodeDemoteCmd)
	rootCmd.AddCommand(nodeCmd)
}

// dockerNewFn is the docker client factory used by openDocker. Package var so
// tests can stub it with an in-memory fake.
var dockerNewFn = docker.New

func openDocker() (runtime.Client, error) {
	dc, err := dockerNewFn()
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return dc, nil
}

func runNodeList(cmd *cobra.Command, _ []string) error {
	dc, err := openDocker()
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()

	nodes, err := dc.NodeList(cmd.Context())
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no nodes — is Swarm initialised?)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOSTNAME\tROLE\tSTATUS\tAVAILABILITY\tLEADER\tENGINE\tID")
	for _, n := range nodes {
		leader := ""
		if n.IsLeader {
			leader = "★"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.Hostname, n.Role, n.Status, n.Availability, leader, n.EngineVersion, shortID(n.ID),
		)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, n := range nodes {
		if n.IsLeader {
			fmt.Fprintf(cmd.OutOrStdout(), "\nLeader address: %s (joined %s)\n",
				n.Address, time.Unix(n.CreatedAt, 0).Format(time.RFC3339))
		}
	}
	return nil
}

func runNodeJoinToken(cmd *cobra.Command, args []string) error {
	role := "worker"
	if len(args) == 1 {
		role = args[0]
	}
	if role != "worker" && role != "manager" {
		return errors.New("role must be 'worker' or 'manager'")
	}

	dc, err := openDocker()
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()

	tokens, err := dc.JoinTokens(cmd.Context())
	if err != nil {
		return err
	}
	nodes, err := dc.NodeList(cmd.Context())
	if err != nil {
		return err
	}

	var leaderAddr string
	for _, n := range nodes {
		if n.IsLeader && n.Address != "" {
			leaderAddr = n.Address
			break
		}
	}

	tok := tokens.Worker
	if role == "manager" {
		tok = tokens.Manager
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "On the new %s node, run:\n\n", role)
	if leaderAddr == "" {
		fmt.Fprintf(out, "  docker swarm join --token %s <MANAGER_IP>:2377\n", tok)
		fmt.Fprintln(out, "\n(could not auto-detect manager address — fill in <MANAGER_IP> manually)")
	} else {
		fmt.Fprintf(out, "  docker swarm join --token %s %s\n", tok, leaderAddr)
	}
	return nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// findNodeByHostname returns the swarm node with the given hostname.
func findNodeByHostname(nodes []runtime.Node, hostname string) (runtime.Node, error) {
	for _, n := range nodes {
		if n.Hostname == hostname {
			return n, nil
		}
	}
	return runtime.Node{}, fmt.Errorf("node %q not found in swarm (see: pmcluster node list)", hostname)
}

// readStorageNodes returns the current storage_nodes cluster setting.
func readStorageNodes(cmd *cobra.Command) (string, error) {
	st, _, err := openStore()
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()
	return st.GetSettingDefault(cmd.Context(), cluster.SettingStorageNodes(), ""), nil
}

func writeStorageNodes(cmd *cobra.Command, value string) error {
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return st.SetSetting(cmd.Context(), cluster.SettingStorageNodes(), value)
}

func runNodePromote(cmd *cobra.Command, args []string) error {
	hostname := args[0]

	dc, err := openDocker()
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()

	nodes, err := dc.NodeList(cmd.Context())
	if err != nil {
		return err
	}
	node, err := findNodeByHostname(nodes, hostname)
	if err != nil {
		return err
	}

	cur, err := readStorageNodes(cmd)
	if err != nil {
		return err
	}
	merged, changed := appendStorageNode(cur, hostname)
	if err := writeStorageNodes(cmd, merged); err != nil {
		return err
	}

	if err := dc.SetNodeLabel(cmd.Context(), node.ID, runtime.StorageNodeLabel, "true"); err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "⚠ updated storage_nodes=%s but the pmcluster.storage label could not be stamped: %v\n", merged, err)
		fmt.Fprintln(cmd.OutOrStdout(), "  run: pmcluster cluster update   (the label-repair step stamps it)")
		return nil
	}

	if !changed {
		fmt.Fprintf(cmd.OutOrStdout(), "%s is already a storage node (storage_nodes=%s)\n", hostname, merged)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ %s is now a storage node (storage_nodes=%s)\n", hostname, merged)
	fmt.Fprintln(cmd.OutOrStdout(), "  The backup agent runs a global task on every storage node; new stateful")
	fmt.Fprintln(cmd.OutOrStdout(), "  stacks may round-robin onto it. Move existing stacks with:")
	fmt.Fprintln(cmd.OutOrStdout(), "    pmcluster stack move <stack> --to "+hostname)
	return nil
}

func runNodeDemote(cmd *cobra.Command, args []string) error {
	hostname := args[0]

	dc, err := openDocker()
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()

	nodes, err := dc.NodeList(cmd.Context())
	if err != nil {
		return err
	}
	node, err := findNodeByHostname(nodes, hostname)
	if err != nil {
		return err
	}

	cur, err := readStorageNodes(cmd)
	if err != nil {
		return err
	}
	var kept []string
	removed := false
	for _, p := range strings.Split(cur, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == hostname {
			removed = true
			continue
		}
		kept = append(kept, p)
	}
	merged := strings.Join(kept, ",")
	if err := writeStorageNodes(cmd, merged); err != nil {
		return err
	}
	if err := dc.SetNodeLabel(cmd.Context(), node.ID, runtime.StorageNodeLabel, ""); err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "⚠ storage_nodes updated but the label could not be cleared: %v\n", err)
		return nil
	}

	if !removed {
		fmt.Fprintf(cmd.OutOrStdout(), "%s was not in storage_nodes (label cleared anyway)\n", hostname)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ %s is no longer a storage node (storage_nodes=%s)\n", hostname, merged)
	fmt.Fprintln(cmd.OutOrStdout(), "  Stacks still pinned to it keep running there — move them with:")
	fmt.Fprintln(cmd.OutOrStdout(), "    pmcluster stack move <stack> --to <other-storage-node>")
	return nil
}
