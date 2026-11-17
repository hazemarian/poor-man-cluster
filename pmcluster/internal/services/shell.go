package services

import (
	"context"
	"fmt"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// ShellRequest is what an operator asks for when they open a stack's terminal.
type ShellRequest struct {
	Stack string
	// Image is the client image. It must carry the database clients: a shell without psql or mysql
	// answers the wrong question for a platform whose workloads are mostly databases.
	Image string
	// SecretNames/ConfigNames are the SWARM object names to mount. The caller resolves them from
	// the control-plane store; this layer never reads the store.
	SecretNames []string
	ConfigNames []string
}

// ShellSession identifies a live shell and what to exec into.
type ShellSession = runtime.ShellSession

// OpenShell creates the throwaway container for one terminal session.
//
// The whole point is placement: the container is pinned to THIS node, so the interactive exec that
// follows is a local `docker exec` and never needs a node-to-node channel. Measured cause: the
// console's daemon is the leader, a stack's container is often elsewhere, and exec there failed with
// "No such container" — the terminal could only ever reach services that happened to share a node
// with the console.
func (l Local) OpenShell(ctx context.Context, req ShellRequest) (ShellSession, error) {
	if err := validateStack(req.Stack); err != nil {
		return ShellSession{}, err
	}
	shell, ok := l.Shell()
	if !ok {
		return ShellSession{}, fmt.Errorf("this node cannot create a shell session (no swarm engine access)")
	}
	node, err := shell.SelfHostname(ctx)
	if err != nil {
		return ShellSession{}, err
	}
	nets, err := shell.StackNetworks(ctx, req.Stack)
	if err != nil {
		return ShellSession{}, err
	}
	if len(nets) == 0 {
		return ShellSession{}, fmt.Errorf("stack %q has no running services with networks — a shell would have nothing to reach", req.Stack)
	}
	spec := runtime.ShellServiceSpec{
		Stack:       req.Stack,
		Image:       req.Image,
		Networks:    nets,
		SecretNames: req.SecretNames,
		ConfigNames: req.ConfigNames,
		Node:        node,
		Life:        runtime.ShellMaxLife,
	}
	id, err := shell.ShellCreate(ctx, spec)
	if err != nil {
		return ShellSession{}, err
	}
	// The container is NOT waited for here. Pulling the shell image takes longer than an HTTP
	// request may last — measured: the first pull of pmcluster-shell blew the daemon's request
	// deadline and the operator got "context deadline exceeded" with a healthy container behind it.
	// The socket waits instead (ShellContainer), where there is no request deadline and the page can
	// say "starting".
	return ShellSession{ServiceID: id, Node: node}, nil
}

// ReapStaleShells removes sessions that outlived their welcome.
//
// Two leaks this closes, both real:
//
//   - a console that died without closing its socket leaves a SERVICE behind. The container's own
//     `sleep` ends it at ShellMaxLife, but the orchestrator then restarts the task, forever;
//   - a session whose teardown failed is never retried by anyone.
//
// Only sessions older than max are touched, and that age comes from the label the creator stamped —
// never from "it looks idle", because an operator typing for twenty minutes is indistinguishable
// from an abandoned shell.
func ReapStaleShells(ctx context.Context, rt runtime.ShellRuntime, now time.Time, max time.Duration) (int, error) {
	refs, err := rt.ShellList(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	var firstErr error
	for _, ref := range refs {
		if ref.Created.IsZero() || now.Sub(ref.Created) < max {
			continue
		}
		if rerr := rt.ShellRemove(ctx, ref.ServiceID); rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

// ShellContainer resolves the running container for a session, waiting for the task to start.
func (l Local) ShellContainer(ctx context.Context, serviceID, node string) (string, error) {
	shell, ok := l.Shell()
	if !ok {
		return "", fmt.Errorf("this node cannot exec into a container")
	}
	return shell.ShellContainerOn(ctx, serviceID, node)
}

// CloseShell destroys the session. Called when the socket closes, for any reason.
func (l Local) CloseShell(ctx context.Context, serviceID string) error {
	if serviceID == "" {
		return nil
	}
	shell, ok := l.Shell()
	if !ok {
		return nil
	}
	return shell.ShellRemove(ctx, serviceID)
}

// Shell returns the shell capability of this node's engine, if it has one.
func (l Local) Shell() (runtime.ShellRuntime, bool) {
	s, ok := l.Docker.(runtime.ShellRuntime)
	return s, ok
}
