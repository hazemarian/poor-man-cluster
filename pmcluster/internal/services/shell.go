package services

import (
	"context"
	"fmt"

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
	cid, err := shell.ShellContainerOn(ctx, id, node)
	if err != nil {
		// Do not leave a container behind because the wait failed.
		_ = shell.ShellRemove(context.WithoutCancel(ctx), id)
		return ShellSession{}, err
	}
	return ShellSession{ServiceID: id, ContainerID: cid, Node: node}, nil
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
