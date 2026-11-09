package stacks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

// isPlacementRole reports whether a placement value is a swarm role rather than
// a node hostname. The DSL treats every value that is not one of these as a
// hostname and renders `node.hostname == <value>`, so this must agree with
// internal/engine.PlacementFor: a node actually named "manager" is
// indistinguishable from the role, and the role interpretation wins (the same
// value renders the same constraint either way, so a mismatch here can never
// turn a valid manifest into a refused one).
func isPlacementRole(v string) bool {
	return v == "manager" || v == "worker"
}

// checkPlacementHosts refuses a manifest that pins a service to a hostname the
// swarm does not have.
//
// A placement typo is silent otherwise: the constraint `node.hostname == typo`
// matches no node, the service is created, and Swarm reports 0/N tasks forever
// with no rejection and no log line at deploy time. The operator finds out by
// noticing that an app they just deployed never came up.
//
// Role keywords (manager, worker) and an empty placement are not hostnames and
// are skipped: they name a class of node, not one node, and the scheduler
// resolves them.
func checkPlacementHosts(app *dsl.App, nodes []runtime.Node) error {
	known := make([]string, 0, len(nodes))
	have := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if strings.TrimSpace(n.Hostname) == "" {
			continue
		}
		known = append(known, n.Hostname)
		have[n.Hostname] = true
	}
	sort.Strings(known)

	// Group by service name in sorted order so the message names the same
	// services in the same order on every run (map iteration is random).
	names := make([]string, 0, len(app.Services))
	for name := range app.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	var problems []string
	for _, name := range names {
		svc := app.Services[name]
		if svc == nil {
			continue
		}
		host := strings.TrimSpace(svc.Placement)
		if host == "" || isPlacementRole(host) || have[host] {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s.placement: unknown swarm node %q", prefixFor(name), host))
	}
	if len(problems) == 0 {
		return nil
	}

	where := "the cluster has no nodes"
	if len(known) > 0 {
		where = "the cluster's nodes are " + strings.Join(known, ", ")
	}
	return fmt.Errorf("%s — %s; a misspelt hostname matches no node, so the service could never be scheduled (use a hostname from that list, the manager/worker role, or no placement)",
		strings.Join(problems, "; "), where)
}

func prefixFor(service string) string { return "services." + service }

// validatePlacementHosts is the deploy-time gate: it reads the swarm's node list
// and refuses hostname pins that name a node which is not there.
//
// It is skipped when there is no Docker client (the CLI path and most tests) and
// when no service uses a hostname pin — the common case costs no API call. A
// node-list read error is NOT a bad hostname: the deploy proceeds with a
// warning, because failing here would block a legitimate deploy on a transient
// swarm-API hiccup, which is the failure this whole change exists to avoid.
func (s *Service) validatePlacementHosts(ctx context.Context, app *dsl.App) error {
	if s.Docker == nil || app == nil {
		return nil
	}
	if !usesHostnamePlacement(app) {
		return nil
	}
	nodes, err := s.Docker.NodeList(ctx)
	if err != nil {
		s.Log.Warn().Err(err).Str("stack", app.Name).
			Msg("deploy — could not list swarm nodes, skipping the placement-host check")
		return nil
	}
	return checkPlacementHosts(app, nodes)
}

// usesHostnamePlacement reports whether any service pins itself to a node
// hostname (as opposed to a role or nothing).
func usesHostnamePlacement(app *dsl.App) bool {
	for _, svc := range app.Services {
		if svc == nil {
			continue
		}
		if v := strings.TrimSpace(svc.Placement); v != "" && !isPlacementRole(v) {
			return true
		}
	}
	return false
}
