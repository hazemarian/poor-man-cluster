package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/spf13/cobra"
)

// clusterOrphansRemove turns the report into a deletion. Off by default: the
// command is a read until the operator says otherwise.
var clusterOrphansRemove bool

// clusterOrphansCmd reports services the swarm runs that belong to no stack.
//
// They are what a half-finished `docker stack deploy`, a hand-run
// `docker service create`, or a stack removed with the docker CLI instead of
// the API leaves behind: invisible to `docker stack ls`, absent from
// `pmcluster stack list`, and never reconciled — so nothing scales them,
// updates them, or cleans them up, and their tasks keep consuming the node.
//
// A service belongs to a stack by carrying com.docker.stack.namespace; the
// runtime client already surfaces that as Service.Stack ("" when absent), so
// the test needs no label parsing here. Only stack-less services are ever
// candidates: a labelled service is left alone even when its stack no longer
// exists, because removing it is the stack's own re-deploy to settle.
var clusterOrphansCmd = &cobra.Command{
	Use:   "orphans",
	Short: "List services that belong to no stack (and remove them with --remove)",
	Long: `List every swarm service that carries no stack namespace — leftovers from a
half-finished deploy, a hand-created service, or a stack removed with the docker
CLI. Re-run with --remove to delete them.

A service that DOES carry a stack label is never touched, even if its stack is
gone: that service belongs to a deploy that is still converging.`,
	RunE: runClusterOrphans,
}

func init() {
	clusterOrphansCmd.Flags().BoolVar(&clusterOrphansRemove, "remove", false,
		"delete the orphan services instead of only listing them")
}

func runClusterOrphans(cmd *cobra.Command, _ []string) error {
	dc, err := openDocker()
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()

	services, err := dc.ServiceList(cmd.Context())
	if err != nil {
		return err
	}

	orphans := orphanServices(services)
	if len(orphans) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No orphan services: every service carries a stack namespace.")
		return nil
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%-34s %-11s %-14s %s\n", "SERVICE", "REPLICAS", "MODE", "IMAGE")
	for _, s := range orphans {
		fmt.Fprintf(out, "%-34s %-11s %-14s %s\n",
			s.Name, replicaState(s), modeOrDash(s.Mode), s.Image)
	}

	if !clusterOrphansRemove {
		fmt.Fprintf(out, "\n%d orphan service(s). Re-run with --remove to delete them.\n", len(orphans))
		return nil
	}

	// Sequential and reported per service: a removal that fails must not hide
	// the ones that succeeded, and the exit code has to be non-zero so a
	// scripted cleanup cannot pass on a partial result.
	var failed int
	for _, s := range orphans {
		if err := removeService(cmd.Context(), s.Name); err != nil {
			fmt.Fprintf(out, "✗ %s: %v\n", s.Name, err)
			failed++
			continue
		}
		fmt.Fprintf(out, "✓ removed %s\n", s.Name)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d orphan service(s) could not be removed", failed, len(orphans))
	}
	return nil
}

// orphanServices returns the services with no stack namespace, preserving the
// client's order so the report is stable between runs.
func orphanServices(services []runtime.Service) []runtime.Service {
	out := make([]runtime.Service, 0, len(services))
	for _, s := range services {
		if s.Stack == "" {
			out = append(out, s)
		}
	}
	return out
}

// removeService deletes one service through the docker CLI, the same way the
// deployer removes a whole stack — the runtime client has no service-remove
// method, and adding one for a maintenance command would widen an interface
// every fake has to satisfy.
func removeService(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, "docker", "service", "rm", name).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("docker service rm: %s", msg)
		}
		return fmt.Errorf("docker service rm: %w", err)
	}
	return nil
}

// replicaState renders a service's replicas the way the operator reads them:
// running/desired, with the one-shot case named instead of shown as 0/1.
func replicaState(s runtime.Service) string {
	state := fmt.Sprintf("%d/%d", s.Replicas, s.Desired)
	if s.RunOnce && s.Replicas == 0 {
		return state + " (run-once)"
	}
	return state
}

func modeOrDash(mode string) string {
	if mode == "" {
		return "—"
	}
	return mode
}
