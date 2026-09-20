package cli

import (
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

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

func init() {
	nodeCmd.AddCommand(nodeListCmd, nodeJoinTokenCmd)
	rootCmd.AddCommand(nodeCmd)
}

func openDocker() (runtime.Client, error) {
	dc, err := docker.New()
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
