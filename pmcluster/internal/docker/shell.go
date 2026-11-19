package docker

import (
	"context"
	"fmt"
	"strconv"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// A shell's writable space is capped, and the cap is on /tmp — that is where an operator puts a
// dump before restoring it, and it is the only place in the container anything large is written.
//
// The alternative, capping the container's writable LAYER (storage-opt size), is not portable: it
// needs a filesystem and driver combination most of these hosts do not have, and when unsupported
// the container is refused rather than limited. A sized tmpfs works everywhere and moves the cost
// from the node's disk (shared with the control plane) to memory that the container's own limit
// bounds — so an oversized download fails inside the shell instead of filling a manager.
const (
	// ShellTmpfsBytes is the size of /tmp in a shell session: enough for a compressed database
	// dump, not enough to matter to the host.
	ShellTmpfsBytes = 200 << 20 // 200 MiB
	// ShellMemoryBytes leaves room for the tmpfs plus the clients (psql, pg_restore, curl) and
	// their buffers. A session that exceeds it is OOM-killed, which is the correct blast radius:
	// one throwaway container, not the node.
	ShellMemoryBytes = 512 << 20 // 512 MiB
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

	// A file-target mount needs the object's ID as well as its name: the engine refuses a
	// name-only reference with "malformed secret reference". The mount NAME is the operator-facing
	// one — the swarm object is <name>_<hash8>, and /run/secrets/<name>_<hash8> would be a puzzle
	// where /run/secrets/<name> is what every stack's service already sees.
	mounts := make([]*swarm.SecretReference, 0, len(spec.SecretNames))
	for _, n := range spec.SecretNames {
		meta, _, ierr := r.c.SecretInspectWithRaw(ctx, n)
		if ierr != nil {
			return "", fmt.Errorf("shell: secret %s is not in the swarm: %w", n, ierr)
		}
		mounts = append(mounts, &swarm.SecretReference{
			SecretID:   meta.ID,
			SecretName: n,
			File:       &swarm.SecretReferenceFileTarget{Name: mountName(n), UID: "0", GID: "0", Mode: 0o400},
		})
	}
	cfgMounts := make([]*swarm.ConfigReference, 0, len(spec.ConfigNames))
	for _, n := range spec.ConfigNames {
		meta, _, ierr := r.c.ConfigInspectWithRaw(ctx, n)
		if ierr != nil {
			return "", fmt.Errorf("shell: config %s is not in the swarm: %w", n, ierr)
		}
		cfgMounts = append(cfgMounts, &swarm.ConfigReference{
			ConfigID:   meta.ID,
			ConfigName: n,
			// A config's File.Name is its path RELATIVE TO /, unlike a secret's, which the runtime
			// places under /run/secrets. Measured: Name "aarma_admin_phones" landed the file at
			// /aarma_admin_phones — root of the container, one typo away from clobbering /bin.
			File: &swarm.ConfigReferenceFileTarget{
				Name: "run/configs/" + mountName(n), UID: "0", GID: "0", Mode: 0o444,
			},
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
				// HOME for psql's history, and NO PGHOST: a default host is a guess, and a guess
				// that happens to resolve connects an operator to the wrong database quietly.
				// Without it, psql fails immediately (no local socket) and says so, which is the
				// correct answer to "which database?" — the operator names it: `psql -h aarma_db`.
				Env:     ShellEnv(),
				Secrets: mounts,
				Configs: cfgMounts,
				// Downloads land in /tmp; give it a ceiling that belongs to the session.
				Mounts: shellMounts(),
				// A shell is interactive; keep it readable and writable by root only.
				User: "0:0",
				TTY:  false,
			},
			// The sized tmpfs and the memory limit travel together: without the limit, a 200 MiB
			// tmpfs is a way to pressure the node's memory instead of its disk.
			Resources: &swarm.ResourceRequirements{
				Limits: &swarm.Limit{MemoryBytes: ShellMemoryBytes},
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
		// Already gone is success. The session's socket close removes the service, so a caller that
		// also deletes it — the console's onclose, a script, the sweeper racing a close — arrives a
		// moment too late and must not be told it failed.
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("remove shell service: %w", err)
	}
	return nil
}

// shellMounts is the session's only mount: a sized tmpfs on /tmp, which is where an operator puts a
// dump. It is a function so the mount and the constant cannot drift apart — the tests read it.
func shellMounts() []mount.Mount {
	return []mount.Mount{{
		Type:   mount.TypeTmpfs,
		Target: "/tmp",
		TmpfsOptions: &mount.TmpfsOptions{
			SizeBytes: ShellTmpfsBytes,
			Mode:      0o1777, // as /tmp should be: sticky, writable inside the shell
		},
	}}
}

// ShellEnv returns the environment a shell session gets. It exists so the absence of PGHOST is a
// tested decision rather than a missing line: a default host that resolves is worse than none,
// because it connects the operator to whatever answers first.
func ShellEnv() []string {
	return []string{"HOME=/root", "PS1=\\w \\$ "}
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

// ShellList returns the shell sessions this node still holds. The labels live on the service spec,
// so the created stamp is read from there rather than from the task.
func (r *realClient) ShellList(ctx context.Context) ([]runtime.ShellRef, error) {
	services, err := r.c.ServiceList(ctx, swarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", runtime.ShellLabel+"=true")),
	})
	if err != nil {
		return nil, fmt.Errorf("list shell services: %w", err)
	}
	out := make([]runtime.ShellRef, 0, len(services))
	for _, svc := range services {
		ref := runtime.ShellRef{ServiceID: svc.ID}
		if svc.Spec.Labels != nil {
			ref.Stack = svc.Spec.Labels[runtime.ShellStackLabel]
			if secs, perr := strconv.ParseInt(svc.Spec.Labels[runtime.ShellCreatedLabel], 10, 64); perr == nil {
				ref.Created = time.Unix(secs, 0)
			}
		}
		out = append(out, ref)
	}
	return out, nil
}

// mountName is the file name a mounted swarm object gets inside the shell: the swarm name without
// its content hash, so an operator finds /run/secrets/<name> and not /run/secrets/<name>_a1b2c3d4.
func mountName(swarmName string) string {
	if i := len(swarmName) - 9; i > 0 && swarmName[i] == '_' {
		hash := swarmName[i+1:]
		isHex := true
		for _, c := range hash {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				isHex = false
				break
			}
		}
		if isHex {
			return swarmName[:i]
		}
	}
	return swarmName
}

func shellName(stack string) string {
	return fmt.Sprintf("pmshell-%s-%d", stack, time.Now().UnixNano()%1e6)
}

func ptr[T any](v T) *T { return &v }

// keep the container/mount imports honest if the spec shape changes
var _ = container.HostConfig{}
var _ = mount.Mount{}
