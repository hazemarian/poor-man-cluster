package docker

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// ShellCreate creates the throwaway container an operator's terminal session runs in.
//
// It is a SERVICE, not `docker run`, for one measured reason: a stack's networks are created by
// swarm without `attachable`, so `docker run --network <stack>_net` is refused with
// "network … not manually attachable". A service may join them, and the placement constraint pins it
// to this node so the exec that follows stays local.
func (r *realClient) ShellCreate(ctx context.Context, spec runtime.ShellServiceSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	life := spec.Life
	if life <= 0 {
		life = runtime.ShellMaxLife
	}
	name := shellName(spec.Stack)

	mounts := make([]*swarm.SecretReference, 0, len(spec.SecretNames))
	for _, n := range spec.SecretNames {
		mounts = append(mounts, &swarm.SecretReference{
			SecretName: n,
			File:       &swarm.SecretReferenceFileTarget{Name: n, UID: "0", GID: "0", Mode: 0o400},
		})
	}
	cfgMounts := make([]*swarm.ConfigReference, 0, len(spec.ConfigNames))
	for _, n := range spec.ConfigNames {
		cfgMounts = append(cfgMounts, &swarm.ConfigReference{
			ConfigName: n,
			File:       &swarm.ConfigReferenceFileTarget{Name: n, UID: "0", GID: "0", Mode: 0o444},
		})
	}

	// `sleep` keeps the container alive for exactly the session's ceiling: the task exits on its own
	// if nobody closes the socket, and the sweeper is the backstop behind that.
	cmd := []string{"sh", "-c", fmt.Sprintf("sleep %d", int(life.Seconds()))}

	svc := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: name,
			Labels: map[string]string{
				runtime.ShellLabel:        "true",
				runtime.ShellStackLabel:   spec.Stack,
				runtime.ShellCreatedLabel: fmt.Sprintf("%d", time.Now().Unix()),
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:   spec.Image,
				Command: cmd,
				// The clients need a working /tmp and HOME=/root for psql's history.
				Env:     []string{"HOME=/root", "PGHOST=" + spec.Stack + "_db", "PS1=\\w \\$ "},
				Secrets: mounts,
				Configs: cfgMounts,
				// A shell is interactive; keep it readable and writable by root only.
				User: "0:0",
				TTY:  false,
			},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone},
			Placement: &swarm.Placement{
				Constraints: []string{"node.hostname==" + spec.Node},
			},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: ptr(uint64(1))}},
	}
	for _, netID := range spec.Networks {
		svc.TaskTemplate.Networks = append(svc.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{
			Target: netID,
		})
	}

	created, err := r.c.ServiceCreate(ctx, svc, swarm.ServiceCreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create shell service %s: %w", name, err)
	}
	return created.ID, nil
}

// ShellContainerOn waits for the shell service's task to be RUNNING on node and returns its
// container ID there. Bounded: a scheduler that cannot place the shell (image pull failing, node
// drained) must fail the operator's open with a reason, not hang their browser.
func (r *realClient) ShellContainerOn(ctx context.Context, serviceID, node string) (string, error) {
	deadline := time.Now().Add(45 * time.Second)
	var last string
	for {
		tasks, err := r.c.TaskList(ctx, swarm.TaskListOptions{
			Filters: filters.NewArgs(filters.Arg("service", serviceID)),
		})
		if err != nil {
			return "", fmt.Errorf("task list: %w", err)
		}
		for _, t := range tasks {
			if t.NodeID == "" || t.Status.ContainerStatus == nil {
				continue
			}
			switch t.Status.State {
			case swarm.TaskStateRunning:
				if t.Status.ContainerStatus.ContainerID != "" {
					return t.Status.ContainerStatus.ContainerID, nil
				}
			case swarm.TaskStateFailed, swarm.TaskStateRejected:
				last = t.Status.Err
			}
		}
		if time.Now().After(deadline) {
			if last != "" {
				return "", fmt.Errorf("the shell container never started: %s", last)
			}
			return "", fmt.Errorf("the shell container did not start within 45s on %s", node)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ShellRemove deletes the shell service and, with it, its container.
func (r *realClient) ShellRemove(ctx context.Context, serviceID string) error {
	if serviceID == "" {
		return nil
	}
	if err := r.c.ServiceRemove(ctx, serviceID); err != nil {
		return fmt.Errorf("remove shell service: %w", err)
	}
	return nil
}

// StackNetworks returns the network IDs the stack's services are attached to, de-duplicated. A
// shell that joined only one of a stack's networks could not reach the services on the others.
func (r *realClient) StackNetworks(ctx context.Context, stack string) ([]string, error) {
	svcs, err := r.c.ServiceList(ctx, swarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", "com.docker.stack.namespace="+stack)),
	})
	if err != nil {
		return nil, fmt.Errorf("service list for stack %s: %w", stack, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range svcs {
		for _, n := range s.Spec.TaskTemplate.Networks {
			if n.Target != "" && !seen[n.Target] {
				seen[n.Target] = true
				out = append(out, n.Target)
			}
		}
	}
	return out, nil
}

// SelfHostname is the node this daemon runs on: the shell is pinned here so the exec is local.
func (r *realClient) SelfHostname(ctx context.Context) (string, error) {
	info, err := r.c.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("docker info: %w", err)
	}
	if info.Name == "" {
		return "", fmt.Errorf("docker info reported no node name")
	}
	return info.Name, nil
}

func shellName(stack string) string {
	return fmt.Sprintf("pmshell-%s-%d", stack, time.Now().UnixNano()%1e6)
}

func ptr[T any](v T) *T { return &v }

// keep the container/mount imports honest if the spec shape changes
var _ = container.HostConfig{}
var _ = mount.Mount{}
