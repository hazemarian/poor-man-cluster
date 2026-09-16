package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
)

// bundledServices is the canonical list of services deployed by the four
// bundled stacks. Used by WaitHealthyStacks to verify every service has at
// least one running replica after cluster up.
var bundledServices = []string{
	"infra_traefik",
	"infra_portainer",
	"edge_pmcluster-edge",
	"observability_openobserve",
	"observability_otel-collector",
	"backup_volume-backup",
}

const (
	defaultHealthTimeout  = 60 * time.Second
	defaultHealthInterval = 3 * time.Second
)

// WaitHealthyStacks polls `docker service ls` until every bundled service
// has at least one running replica, or until the deadline expires. On a
// single-node swarm (the typical pmcluster target) this means 1/1 for
// replicated and 1/1 for global services.
//
// When the health check times out, it prints the last 20 log lines from
// each unhealthy service so the operator can diagnose the problem.
//
// Progress is written to w (nil ok — uses io.Discard).
func WaitHealthyStacks(ctx context.Context, d docker.Client, w io.Writer) error {
	if w == nil {
		w = io.Discard
	}

	deadline := time.Now().Add(defaultHealthTimeout)
	ticker := time.NewTicker(defaultHealthInterval)
	defer ticker.Stop()

	var unhealthy []string

	for {
		select {
		case <-ctx.Done():
			printUnhealthyLogs(ctx, unhealthy, w)
			return fmt.Errorf("timed out waiting for services to be healthy: %w", ctx.Err())
		default:
		}

		timedOut := time.Now().After(deadline)

		svcs, err := d.ServiceList(ctx)
		if err != nil {
			fmt.Fprintf(w, "  ⚠ service list failed (retrying): %v\n", err)
			select {
			case <-ticker.C:
			case <-ctx.Done():
				printUnhealthyLogs(ctx, unhealthy, w)
				return fmt.Errorf("timed out: %w", ctx.Err())
			}
			continue
		}

		unhealthy = unhealthy[:0]
		for _, s := range svcs {

			isHealthy := s.Desired > 0 && s.Replicas >= s.Desired
			if isHealthy {
				continue
			}

			found := false
			for _, name := range bundledServices {
				if s.Name == name {
					found = true
					break
				}
			}
			if found {
				unhealthy = append(unhealthy, s.Name)
			}
		}

		allHealthy := true
		healthyMap := make(map[string]bool, len(svcs))
		for _, s := range svcs {
			if s.Desired > 0 && s.Replicas >= s.Desired {
				healthyMap[s.Name] = true
			}
		}
		for _, name := range bundledServices {
			if !healthyMap[name] {
				allHealthy = false
				break
			}
		}

		if allHealthy {
			fmt.Fprintln(w, "  ✓ All bundled services are healthy")
			return nil
		}

		if timedOut {
			printUnhealthyLogs(ctx, unhealthy, w)
			return fmt.Errorf("timed out after %v waiting for services to become healthy", defaultHealthTimeout)
		}

		statuses := make([]string, 0, len(bundledServices))
		for _, name := range bundledServices {
			if healthyMap[name] {
				statuses = append(statuses, fmt.Sprintf("%s:✓", name))
				continue
			}
			found := false
			for _, s := range svcs {
				if s.Name == name {
					statuses = append(statuses, fmt.Sprintf("%s:%d/%d", name, s.Replicas, s.Desired))
					found = true
					break
				}
			}
			if !found {
				statuses = append(statuses, fmt.Sprintf("%s:?", name))
			}
		}
		fmt.Fprintf(w, "  Waiting for services: %v\n", statuses)

		select {
		case <-ticker.C:
		case <-ctx.Done():
			printUnhealthyLogs(ctx, unhealthy, w)
			return fmt.Errorf("timed out: %w", ctx.Err())
		}
	}
}

// printUnhealthyLogs fetches the last 20 log lines + task error messages
// from each unhealthy service.
func printUnhealthyLogs(ctx context.Context, unhealthy []string, w io.Writer) {
	if len(unhealthy) == 0 {
		return
	}
	fmt.Fprintln(w, "  ── Unhealthy services diagnostics ──")

	logCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, name := range unhealthy {
		fmt.Fprintf(w, "\n  ─── %s ───\n", name)

		psOut, err := servicePS(logCtx, name, 3)
		if err != nil {
			fmt.Fprintf(w, "  (failed to fetch task info: %v)\n", err)
		} else {
			for _, line := range strings.Split(psOut, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				fmt.Fprintf(w, "  %s\n", line)
			}
		}

		logOut, err := serviceLogs(logCtx, name, 20)
		if err != nil {
			fmt.Fprintf(w, "  (failed to fetch logs: %v)\n", err)
			continue
		}
		hasContent := false
		for _, line := range strings.Split(logOut, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			hasContent = true
			fmt.Fprintf(w, "  │ %s\n", line)
		}
		if !hasContent {
			fmt.Fprintf(w, "  (no container output — task may have failed before starting)\n")
		}
	}
	fmt.Fprintln(w)
}

// servicePS runs `docker service ps --no-trunc --format '{{.Name}} {{.CurrentState}} {{.Error}}' <name>`.
func servicePS(ctx context.Context, name string, limit int) (string, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "service", "ps",
		"--no-trunc",
		"--no-resolve",
		"--format", "{{.Name}}\t{{.CurrentState}}\t{{.Error}}",
		name,
	)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	out := buf.String()

	lines := strings.Split(out, "\n")
	result := make([]string, 0, limit)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		result = append(result, line)
		if len(result) >= limit {
			break
		}
	}
	return strings.Join(result, "\n"), nil
}

// serviceLogs runs `docker service logs --tail N <name>` and returns the
// combined stdout+stderr.
func serviceLogs(ctx context.Context, name string, tail int) (string, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "service", "logs",
		"--tail", fmt.Sprint(tail),
		"--no-trunc",
		"--details",
		name,
	)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}
