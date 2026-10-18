package services

// This adapter is deliberately separate from Local. Local speaks the Swarm
// service API; standalone monitoring runs on ordinary Docker hosts where the
// service API is unavailable or intentionally not initialised. It only reads
// container inventory; telemetry is the bounded log/evidence path. Restart,
// exec, task history, and Docker log tails return explicit unsupported or
// read-only errors even if a caller accidentally wires those handlers back in.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

var errStandaloneReadOnly = errors.New("standalone observer is read-only")

// Standalone reads ordinary Docker containers on this host. Compose labels
// provide the stable project/service identity; unlabeled containers use the
// Docker name under the synthetic "standalone" project. The host itself is a
// separate backend, so no cross-host collision resolution is needed here.
type Standalone struct {
	client *client.Client
}

// NewStandalone opens the local Docker Engine API without requiring Swarm.
func NewStandalone() (*Standalone, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create standalone docker client: %w", err)
	}
	return &Standalone{client: c}, nil
}

func (s *Standalone) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

// List returns one observed row per canonical local service. Desired
// replicas are intentionally unknown: Compose files and hand-created
// containers do not provide a scheduler desired count through this read path.
// Multiple Compose replicas and stopped predecessors are aggregated so
// callers never select an arbitrary container for a canonical service.
func (s *Standalone) List(ctx context.Context, stack string) ([]ServiceSummary, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("standalone docker client is unavailable")
	}
	containers, err := s.client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("docker container ls: %w", err)
	}
	identities := map[string]string{}
	for _, c := range containers {
		row := standaloneSummary(c)
		origin := "container:" + strings.TrimPrefix(firstName(c.Names), "/")
		if c.Labels["com.docker.compose.project"] != "" && c.Labels["com.docker.compose.service"] != "" {
			origin = "compose:" + c.Labels["com.docker.compose.project"] + "\x00" + c.Labels["com.docker.compose.service"]
		}
		if old, ok := identities[row.Name]; ok && old != origin {
			return nil, fmt.Errorf("ambiguous standalone service identity %q", row.Name)
		}
		identities[row.Name] = origin
	}
	out := aggregateStandalone(containers)
	if stack == "" {
		return out, nil
	}
	filtered := out[:0]
	for _, row := range out {
		if row.Stack == stack {
			filtered = append(filtered, row)
		}
	}
	return filtered, nil
}

func aggregateStandalone(containers []container.Summary) []ServiceSummary {
	rows := make(map[string]ServiceSummary, len(containers))
	for _, c := range containers {
		row := standaloneSummary(c)
		if existing, ok := rows[row.Name]; ok {
			existing.Replicas += row.Replicas
			if row.Updated > existing.Updated {
				existing.Updated = row.Updated
			}
			if row.Replicas > 0 {
				existing.UpdateState = "running"
				existing.UpdateError = ""
			} else if existing.Replicas == 0 && existing.UpdateError == "" {
				existing.UpdateState = row.UpdateState
				existing.UpdateError = row.UpdateError
			}
			rows[row.Name] = existing
			continue
		}
		rows[row.Name] = row
	}
	out := make([]ServiceSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Standalone) Tasks(context.Context, string, string) ([]TaskRun, error) {
	return nil, errors.New("standalone task history is unavailable; use telemetry evidence")
}

func (s *Standalone) Logs(context.Context, string, string, int) ([]LogLine, error) {
	return nil, errors.New("standalone service logs are available in telemetry evidence")
}

func (s *Standalone) Restart(context.Context, string, string) error { return errStandaloneReadOnly }

func (s *Standalone) Exec(context.Context, string, string, []string) (*ExecResult, error) {
	return nil, errStandaloneReadOnly
}

func standaloneSummary(c container.Summary) ServiceSummary {
	name := strings.TrimPrefix(firstName(c.Names), "/")
	stack, service := standaloneIdentity(c.Labels, name)
	state := string(c.State)
	if state == "" {
		state = "unknown"
	}
	running := uint64(0)
	if c.State == container.StateRunning {
		running = 1
	}
	return ServiceSummary{
		Name:         stack + "_" + service,
		Stack:        stack,
		Replicas:     running,
		Desired:      0,
		Image:        c.Image,
		Mode:         "container",
		ImageCreated: 0,
		Updated:      c.Created,
		UpdateState:  state,
		UpdateError:  c.Status,
		RunningNode:  "local",
		Standalone:   true,
		DesiredKnown: false,
		Platform:     false,
	}
}

func standaloneIdentity(labels map[string]string, name string) (stack, service string) {
	project := strings.TrimSpace(labels["com.docker.compose.project"])
	composeService := strings.TrimSpace(labels["com.docker.compose.service"])
	if project != "" && composeService != "" {
		return project, composeService
	}
	return "standalone", name
}

func firstName(names []string) string {
	if len(names) == 0 {
		return "unknown"
	}
	return names[0]
}

var _ Service = (*Standalone)(nil)
