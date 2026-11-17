package runtime

import (
	"context"
	"fmt"
	"time"
)

// The on-demand shell: a throwaway container, created when an operator opens a stack's terminal and
// destroyed when that socket closes.
//
// Why a container instead of exec'ing into the service: `docker exec` can only run where the
// container is, and the console's daemon is the leader — measured on production, `aarma_db` ran on
// another manager and every terminal attempt died on "No such container". A shell service is
// scheduled onto the daemon's OWN node, joined to the stack's networks, with the stack's secrets and
// configs mounted, so it can reach every service of that stack by name and the exec stays local.
const (
	// ShellLabel marks a container that exists only for an operator's terminal session. Anything
	// carrying it may be destroyed without asking, and the sweeper looks for nothing else.
	ShellLabel = "pmcluster.shell"
	// ShellStackLabel records which stack the session belongs to, so a stray shell can be traced
	// and so the sweeper can tell an operator what it removed.
	ShellStackLabel = "pmcluster.shell.stack"
	// ShellCreatedLabel is the unix timestamp of creation: the sweeper's clock.
	ShellCreatedLabel = "pmcluster.shell.created"

	// ShellMaxLife bounds a session. The service sleeps this long and exits; the sweeper removes
	// anything older, so a console that crashed mid-session cannot leave a shell behind.
	ShellMaxLife = 30 * time.Minute
)

// ShellServiceSpec is everything needed to create one shell session.
type ShellServiceSpec struct {
	// Stack names the stack whose networks, secrets and configs the shell receives.
	Stack string
	// Image is the client image (it must carry the database clients an operator expects).
	Image string
	// Networks are the swarm network IDs the stack's services are attached to.
	Networks []string
	// SecretNames and ConfigNames are the SWARM object names to mount under /run/secrets/<name>
	// and /run/configs/<name>. The caller resolves them from the control-plane store.
	SecretNames []string
	ConfigNames []string
	// Node pins the task to this hostname, so the exec is local to the daemon that created it.
	Node string
	// Life is how long the container may exist at all.
	Life time.Duration
}

// ShellSession is what the caller needs to open a socket and to clean up.
type ShellSession struct {
	ServiceName string
	ServiceID   string
	ContainerID string
	// Node is the hostname the task landed on. Equal to the requested node by construction; kept
	// because a shell on the wrong node is a bug worth reporting rather than hiding.
	Node string
}

// Validate refuses a spec the engine would accept but the platform should not.
func (s ShellServiceSpec) Validate() error {
	if s.Stack == "" {
		return fmt.Errorf("shell: stack is required")
	}
	if s.Image == "" {
		return fmt.Errorf("shell: image is required")
	}
	if len(s.Networks) == 0 {
		return fmt.Errorf("shell: the stack has no networks to join — a shell with nothing to reach is a container with no purpose")
	}
	if s.Node == "" {
		return fmt.Errorf("shell: node is required (the session must run where the daemon can exec into it)")
	}
	return nil
}

// ShellRuntime is the slice of the engine the on-demand shell needs.
//
// Separate from Client on purpose: Client is implemented by fakes across the test suite, and adding
// three methods to it would make every one of them grow stubs for calls they never make.
type ShellRuntime interface {
	// ShellCreate creates the service and returns its ID.
	ShellCreate(ctx context.Context, spec ShellServiceSpec) (string, error)
	// ShellContainerOn waits (bounded) for the service's running task on node and returns the
	// container ID there.
	ShellContainerOn(ctx context.Context, serviceID, node string) (string, error)
	// ShellRemove deletes the service. Called when the socket closes, whatever the reason.
	ShellRemove(ctx context.Context, serviceID string) error
	// StackNetworks returns the swarm network IDs the stack's services are attached to.
	StackNetworks(ctx context.Context, stack string) ([]string, error)
	// SelfHostname is the node this daemon runs on, which is where the shell must be scheduled.
	SelfHostname(ctx context.Context) (string, error)
	// ShellList returns every shell session this node still has, for the sweeper. A session whose
	// console died without closing its socket stops running at ShellMaxLife, but its SERVICE stays
	// and the orchestrator keeps restarting it — so something has to collect them.
	ShellList(ctx context.Context) ([]ShellRef, error)
}

// ShellRef is one leftover session, as the sweeper sees it.
type ShellRef struct {
	ServiceID string
	Stack     string
	Created   time.Time
}
