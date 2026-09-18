package services

import (
	"context"
	"fmt"
	"regexp"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
)

// stackNameRe matches stack names as produced by the deploy pipeline.
var stackNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Local is the node-side adapter: it talks to the Docker daemon through the
// whitelisted docker.Client surface. Every call resolves the full swarm
// service name from the (stack, service) pair and verifies the service really
// belongs to that stack before acting.
type Local struct {
	Docker docker.Client
}

// List returns every swarm service, optionally filtered to one stack's
// namespace.
func (l Local) List(ctx context.Context, stack string) ([]ServiceSummary, error) {
	if stack != "" {
		if err := validateStack(stack); err != nil {
			return nil, err
		}
	}
	svcs, err := l.Docker.ServiceList(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceSummary, 0, len(svcs))
	for _, s := range svcs {
		if stack != "" && s.Stack != stack {
			continue
		}
		out = append(out, ServiceSummary{
			Name:     s.Name,
			Stack:    s.Stack,
			Replicas: s.Replicas,
			Desired:  s.Desired,
			Image:    s.Image,
			Mode:     s.Mode,
			Updated:  s.UpdatedAt,
		})
	}
	return out, nil
}

// Tasks returns the task (crash/restart) history for one service.
func (l Local) Tasks(ctx context.Context, stack, service string) ([]TaskRun, error) {
	id, err := l.resolve(ctx, stack, service)
	if err != nil {
		return nil, err
	}
	tasks, err := l.Docker.ServiceTasks(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]TaskRun, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, TaskRun{
			TaskID:     t.TaskID,
			Node:       t.Node,
			Slot:       t.Slot,
			State:      t.State,
			Error:      t.Error,
			StartedAt:  t.StartedAt,
			FinishedAt: t.FinishedAt,
		})
	}
	return out, nil
}

// Logs tails a service's stdout/stderr (tail clamped to [1, 2000]).
func (l Local) Logs(ctx context.Context, stack, service string, tail int) ([]LogLine, error) {
	if tail < 1 {
		tail = 1
	}
	if tail > 2000 {
		tail = 2000
	}
	id, err := l.resolve(ctx, stack, service)
	if err != nil {
		return nil, err
	}
	lines, err := l.Docker.ServiceLogs(ctx, id, tail)
	if err != nil {
		return nil, err
	}
	out := make([]LogLine, 0, len(lines))
	for _, ln := range lines {
		out = append(out, LogLine{Stream: ln.Stream, Line: ln.Line})
	}
	return out, nil
}

// Restart forces a rolling restart of a service.
func (l Local) Restart(ctx context.Context, stack, service string) error {
	id, err := l.resolve(ctx, stack, service)
	if err != nil {
		return err
	}
	return l.Docker.ServiceRestart(ctx, id)
}

// Exec runs a fixed argv non-interactively in the service's first running
// task reachable from this node.
func (l Local) Exec(ctx context.Context, stack, service string, argv []string) (*ExecResult, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("argv: required (e.g. `pmcluster service exec STACK SERVICE -- whoami`)")
	}
	if len(argv) > 16 {
		return nil, fmt.Errorf("argv: at most 16 arguments")
	}
	for _, a := range argv {
		if len(a) > 200 {
			return nil, fmt.Errorf("argv: arguments must be <= 200 bytes")
		}
	}
	id, err := l.resolve(ctx, stack, service)
	if err != nil {
		return nil, err
	}
	res, err := l.Docker.ServiceExec(ctx, id, argv)
	if err != nil {
		return nil, err
	}
	return &ExecResult{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}, nil
}

// resolve maps a (stack, service) pair to the full swarm service ID, refusing
// to act unless the service actually carries the stack's namespace label.
func (l Local) resolve(ctx context.Context, stack, service string) (string, error) {
	if err := validateStack(stack); err != nil {
		return "", err
	}
	if service == "" {
		return "", fmt.Errorf("service: required")
	}
	full := stack + "_" + service
	svc, err := l.Docker.ServiceInspect(ctx, full)
	if err != nil {
		return "", fmt.Errorf("service %q: %w", full, err)
	}
	if svc.Labels[docker.StackNamespaceLabel] != stack {
		return "", fmt.Errorf("service %q does not belong to stack %q", full, stack)
	}
	return svc.ID, nil
}

func validateStack(stack string) error {
	if stack == "" || !stackNameRe.MatchString(stack) {
		return fmt.Errorf("stack %q: must match %s", stack, stackNameRe)
	}
	return nil
}
