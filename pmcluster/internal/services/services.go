// Package services is the bounded context for whitelisted per-service
// operations that previously lived in Portainer: replica health, task
// (crash/restart) history, log tailing, forced restart and non-interactive
// exec.
//
// Security invariant: no raw `docker <anything>` passthrough. Every
// operation maps to a fixed, code-reviewed Docker SDK call behind the
// daemon's existing Bearer auth. The only user-controlled input is a stack
// name, an unqualified service name, an integer tail count, and an argv
// slice for exec.
package services

import "context"

// ServiceSummary is one row of `pmcluster service ps` — replica health.
type ServiceSummary struct {
	Name     string // full swarm service name (stack_service)
	Stack    string // com.docker.stack.namespace label ("" when not stack-managed)
	Replicas uint64 // running tasks
	Desired  uint64 // desired replica count
	Image    string
	Mode     string // "replicated" | "global" | ""
	Updated  int64  // service spec update time (unix)
}

// TaskRun is one row of `docker service ps` — a task's lifecycle state.
type TaskRun struct {
	TaskID     string
	Node       string
	Slot       int64
	State      string // running | failed | shutdown | rejected | ...
	Error      string // non-empty when the task did not reach running
	StartedAt  int64
	FinishedAt int64
}

// ExecResult is the buffered outcome of a non-interactive exec.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Reader is the read side: service list and per-service task history.
type Reader interface {
	// List returns every swarm service (pmcluster-managed or not), filtered
	// by stack when stack is non-empty.
	List(ctx context.Context, stack string) ([]ServiceSummary, error)
	// Tasks returns the task history for one service (crash/restart trail).
	Tasks(ctx context.Context, stack, service string) ([]TaskRun, error)
}

// Ops is the write side. Both operations are single, well-defined Docker SDK
// calls — never a free-form command string.
type Ops interface {
	// Restart forces a rolling restart (bumps ForceUpdate on the spec).
	Restart(ctx context.Context, stack, service string) error
	// Exec runs a fixed argv in the first running task's container,
	// non-interactive.
	Exec(ctx context.Context, stack, service string, argv []string) (*ExecResult, error)
}

// Logs tails a service's stdout/stderr. Full-text search lives in
// OpenObserve; this is a convenience tail for operators.
type Logs interface {
	Logs(ctx context.Context, stack, service string, tail int) ([]LogLine, error)
}

// LogLine is one demultiplexed line of service output.
type LogLine struct {
	Stream string // "stdout" | "stderr"
	Line   string
}

// Service bundles the three ports for the daemon's HTTP handler and the CLI's
// backend switchpoint.
type Service interface {
	Reader
	Ops
	Logs
}
