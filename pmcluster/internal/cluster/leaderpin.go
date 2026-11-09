package cluster

import (
	"context"
	"fmt"
	"io"
	"slices"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// The OTel collector is placed by a HARD constraint on
// node.labels.pmcluster.leader (see embeds/observability-stack.yml). A
// constraint has no schedulable floor: when no node carries the label the task
// never starts, and the only symptom is an empty node-stats panel with nothing in
// the deploy output saying why. These helpers make the label an explicit
// precondition of deploying that stack, and make a placement change actually
// move the running task — the scheduler keeps a running task where it is, so a
// rewritten constraint alone moves nothing.

// noLeaderLabelCondition is the one sentence for the condition underneath the
// deploy refusal and the doctor check, so an operator who reads it in a failed
// update and then runs `pmcluster doctor` reads the same words in both places.
const NoLeaderLabelCondition = "no node carries " + runtime.LeaderNodeLabel + "=true — the leader must claim it before the collector can be pinned"

// noLeaderLabelCondition is the local spelling used inside this package.
const noLeaderLabelCondition = NoLeaderLabelCondition

// RequireLeaderLabel verifies that some node carries pmcluster.leader=true,
// which the collector's placement constraint requires. It is called before the
// observability stack is deployed: deploying without it produces a service that
// sits at 0/1 with no task log to explain it, which looks like a scheduler bug
// rather than a missing label.
func RequireLeaderLabel(ctx context.Context, dc runtime.Client) error {
	if dc == nil {
		return nil // no orchestrator (standalone): nothing is constrained
	}
	nodes, err := dc.NodeList(ctx)
	if err != nil {
		return fmt.Errorf("cannot verify %s=true: %w", runtime.LeaderNodeLabel, err)
	}
	if len(nodes) == 0 {
		// A manager's node list always contains at least itself, so an empty
		// list is a client in transition, not a cluster without a leader.
		// Nothing can schedule against an empty inventory; the deploy below
		// fails on its own terms rather than being misnamed here.
		return nil
	}
	for _, n := range nodes {
		if n.Labels[runtime.LeaderNodeLabel] == "true" {
			return nil
		}
	}
	return fmt.Errorf("%s", noLeaderLabelCondition)
}

// ClaimLeaderLabel stamps pmcluster.leader=true on the current Swarm leader when
// no node carries it, and returns the node that carries it afterwards ("" when
// there is no leader to claim).
//
// `cluster up` calls this before deploying: on a fresh install the daemon that
// normally maintains the label is installed and started AFTER the deploy, so the
// collector's constraint would match nothing on exactly the run that creates it.
// The CLI is a manager here (Preflight requires it), which is enough to label the
// leader; the daemon re-asserts the same value on every leader tick.
func ClaimLeaderLabel(ctx context.Context, dc runtime.Client) (string, error) {
	if dc == nil {
		return "", nil
	}
	nodes, err := dc.NodeList(ctx)
	if err != nil {
		return "", fmt.Errorf("list nodes for the %s label: %w", runtime.LeaderNodeLabel, err)
	}
	for _, n := range nodes {
		if n.Labels[runtime.LeaderNodeLabel] == "true" {
			return n.Hostname, nil
		}
	}
	for _, n := range nodes {
		if !n.IsLeader || n.Hostname == "" {
			continue
		}
		if err := dc.SetNodeLabel(ctx, n.ID, runtime.LeaderNodeLabel, "true"); err != nil {
			return "", fmt.Errorf("claim %s=true on leader %s: %w", runtime.LeaderNodeLabel, n.Hostname, err)
		}
		return n.Hostname, nil
	}
	return "", nil
}

// observabilityCollectorService is the collector's Swarm service name
// (`<stack>_<service>`), the one service this package moves directly.
const observabilityCollectorService = "observability_otel-collector"

// collectorPlacementDoc is the slice of a rendered observability stack that
// decides where the collector runs.
type collectorPlacementDoc struct {
	Services map[string]struct {
		Deploy struct {
			Placement struct {
				Constraints []string `yaml:"constraints"`
				Preferences []struct {
					Spread string `yaml:"spread"`
				} `yaml:"preferences"`
			} `yaml:"placement"`
		} `yaml:"deploy"`
	} `yaml:"services"`
}

// collectorPlacementOf returns the collector's rendered constraints and
// preferences. Both nil when the compose does not carry the service (a stack
// rendered before the collector existed, or a parse failure): the caller must
// not read that as "placement changed".
func collectorPlacementOf(compose []byte) (constraints, preferences []string) {
	if len(compose) == 0 {
		return nil, nil
	}
	var doc collectorPlacementDoc
	if err := yaml.Unmarshal(compose, &doc); err != nil {
		return nil, nil
	}
	svc, ok := doc.Services["otel-collector"]
	if !ok {
		return nil, nil
	}
	for _, p := range svc.Deploy.Placement.Preferences {
		preferences = append(preferences, p.Spread)
	}
	return svc.Deploy.Placement.Constraints, preferences
}

// collectorPlacementChanged reports whether the collector's placement differs
// between two renders of the observability stack. Preferences count as well as
// constraints: replacing the leader preference with a constraint is exactly the
// change that has to force the running task.
func collectorPlacementChanged(before, after []byte) bool {
	if len(before) == 0 {
		// Nothing to compare against (a row written before renders were
		// snapshotted). Offering to force a service whose previous placement is
		// unknown would be a guess, and the deploy right after this may create it.
		return false
	}
	beforeConstraints, beforePrefs := collectorPlacementOf(before)
	afterConstraints, afterPrefs := collectorPlacementOf(after)
	return !slices.Equal(beforeConstraints, afterConstraints) || !slices.Equal(beforePrefs, afterPrefs)
}

// forceCollectorIfPlacementChanged moves the collector after a render changed
// where it belongs.
//
// `docker stack deploy` (and `docker service update`) rewrite the service SPEC;
// neither reschedules a task that is already running and healthy. So a new
// constraint — or a new preference — leaves the collector on its old node, still
// writing the node-stats JSONL onto a disk the leader's daemon does not read. One
// forced restart per real placement change closes that gap.
//
// Best-effort by design: a failed restart is a warning on the apply-record
// channel, not a failed update — the spec it deployed is correct, and the next
// reconcile pass retries the force while the placement keeps differing.
func forceCollectorIfPlacementChanged(ctx context.Context, out io.Writer, dc runtime.Client, before, after []byte) {
	if dc == nil || !collectorPlacementChanged(before, after) {
		return
	}
	if err := dc.ServiceRestart(ctx, observabilityCollectorService); err != nil {
		fmt.Fprintf(out, "  ⚠ collector placement changed but %s was not restarted (%v) — the running task stays on its old node until something forces it\n",
			observabilityCollectorService, err)
		return
	}
	fmt.Fprintf(out, "  ✓ collector placement changed — forced %s onto the new placement\n", observabilityCollectorService)
}
