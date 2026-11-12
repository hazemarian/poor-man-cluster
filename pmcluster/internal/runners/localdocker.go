package runners

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Container is one local docker container as the runner lifecycle sees it. It is
// deliberately small: the name (the identity the platform owns), whether it is
// running, and its bind mounts (which is how the doctor proves a runner is not
// holding the host's raw socket).
type Container struct {
	ID     string
	Name   string
	Image  string
	State  string // running | exited | created | dead
	Binds  []string
	Labels map[string]string
}

// Running reports whether the container has a live process.
func (c Container) Running() bool { return strings.EqualFold(c.State, "running") }

// HasRawSocketMount reports whether the container binds the host's docker socket
// at /var/run/docker.sock — the exposure the socket proxy exists to remove.
func (c Container) HasRawSocketMount() bool {
	for _, b := range c.Binds {
		if strings.HasPrefix(b, "/var/run/docker.sock:") && strings.Contains(b, ":/var/run/docker.sock") {
			return true
		}
	}
	return false
}

// LocalDocker is the node-local container surface the lifecycle needs. The
// production implementation shells out to the docker CLI (the platform already
// does for stack deploy, login and service ps); tests substitute a fake that
// records argv, which is the strongest assertion available for "the runner got
// exactly these mounts and no others".
type LocalDocker interface {
	// Run creates and starts a detached container: argv is Spec.Argv().
	Run(ctx context.Context, argv []string) error
	// Remove force-removes a container by name. Idempotent: a missing container
	// is success.
	Remove(ctx context.Context, name string) error
	// Inspect returns one container. ok=false means it does not exist.
	Inspect(ctx context.Context, name string) (Container, bool, error)
	// Exec runs a command inside a running container and returns its output. Used
	// only to ask the inner daemon whether it is ready yet: a DinD container is
	// "running" long before its dockerd accepts connections, and starting the
	// runner into a socket that is not listening is a job that fails for reasons
	// nobody can see from the workflow.
	ExecIn(ctx context.Context, name string, cmd ...string) ([]byte, error)
	// VolumeSizes reports the size of every volume whose name starts with prefix, as
	// docker's own accounting has it. The caches a runner keeps between jobs are the
	// one thing the platform never removes, so they have to be measurable.
	VolumeSizes(ctx context.Context, prefix string) ([]VolumeSize, error)
	// RemoveVolume removes a named volume. Idempotent, like Remove: a volume that
	// is already gone is success, and a DinD teardown that fails on "no such
	// volume" would leave a runner that can never be cleaned up.
	RemoveVolume(ctx context.Context, name string) error
}

// CLIDocker implements LocalDocker by running the docker binary. Every call is
// bounded by the caller's context; no shell is involved, so a registration token
// in an environment argument is never re-interpreted.
type CLIDocker struct {
	// Bin is the docker binary (default "docker").
	Bin string
	// Exec runs the binary and returns its combined output. Nil uses
	// exec.CommandContext. Tests replace it to assert the argv.
	Exec func(ctx context.Context, bin string, args ...string) ([]byte, error)
}

func (d *CLIDocker) bin() string {
	if d.Bin != "" {
		return d.Bin
	}
	return "docker"
}

func (d *CLIDocker) exec(ctx context.Context, args ...string) ([]byte, error) {
	if d.Exec != nil {
		return d.Exec(ctx, d.bin(), args...)
	}
	out, err := exec.CommandContext(ctx, d.bin(), args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Run executes `docker run -d …` with the argv the Spec built.
func (d *CLIDocker) Run(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("docker run: empty argument vector")
	}
	_, err := d.exec(ctx, argv...)
	return err
}

// Remove executes `docker rm -f <name>`. A container that is already gone is
// success: teardown must be idempotent.
func (d *CLIDocker) Remove(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("docker rm: empty container name")
	}
	if _, err := d.exec(ctx, "rm", "-f", name); err != nil {
		if strings.Contains(err.Error(), "No such container") || strings.Contains(err.Error(), "no such container") {
			return nil
		}
		return err
	}
	return nil
}

// ExecIn executes `docker exec <name> <cmd…>`. Named ExecIn because CLIDocker
// already has an Exec field: the injection hook the tests use to record argv.
// VolumeSizes reads `docker system df -v` and keeps the volumes with the given prefix.
//
// `docker volume inspect` has no size field, so the daemon's own accounting is the only
// place these numbers exist. It is not free — the daemon walks the volumes — which is why
// this is called on demand (the console page, doctor) and never on every reconcile pass.
func (d *CLIDocker) VolumeSizes(ctx context.Context, prefix string) ([]VolumeSize, error) {
	out, err := d.exec(ctx, "system", "df", "-v", "--format", "{{json .Volumes}}")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name string `json:"Name"`
		Size string `json:"Size"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &raw); err != nil {
		return nil, fmt.Errorf("docker system df: %w", err)
	}
	sizes := make([]VolumeSize, 0, len(raw))
	for _, v := range raw {
		if prefix != "" && !strings.HasPrefix(v.Name, prefix) {
			continue
		}
		n, perr := ParseDockerSize(v.Size)
		if perr != nil {
			// One unparsable size must not hide the others: report what can be read.
			continue
		}
		sizes = append(sizes, VolumeSize{Name: v.Name, Bytes: n})
	}
	return sizes, nil
}

func (d *CLIDocker) ExecIn(ctx context.Context, name string, cmd ...string) ([]byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("docker exec: empty container name")
	}
	if len(cmd) == 0 {
		return nil, fmt.Errorf("docker exec %s: empty command", name)
	}
	return d.exec(ctx, append([]string{"exec", name}, cmd...)...)
}

// RemoveVolume executes `docker volume rm <name>`, treating a missing volume as
// success for the same reason Remove does.
func (d *CLIDocker) RemoveVolume(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("docker volume rm: empty volume name")
	}
	if _, err := d.exec(ctx, "volume", "rm", name); err != nil {
		msg := strings.ToLower(err.Error())
		// Docker has two ways of saying "it is not there", and they differ by version:
		// "no such volume" from `volume inspect`, and "volume X not found" from
		// `volume rm`. Only one of them was matched, so a first start on a node — where
		// the dind volume does not exist yet — failed with
		// `docker volume rm pmc-…-docker: volume pmc-…-docker not found` and the runner
		// never started.
		if strings.Contains(msg, "no such volume") || strings.Contains(msg, "not found") {
			return nil
		}
		// "volume is in use" means a container still holds it: teardown removes the
		// containers first, so this is a real ordering bug and must be visible.
		return err
	}
	return nil
}

// Inspect executes `docker inspect` and decodes the fields the lifecycle needs.
func (d *CLIDocker) Inspect(ctx context.Context, name string) (Container, bool, error) {
	if strings.TrimSpace(name) == "" {
		return Container{}, false, fmt.Errorf("docker inspect: empty container name")
	}
	out, err := d.exec(ctx, "inspect", name)
	if err != nil {
		if strings.Contains(err.Error(), "No such object") || strings.Contains(err.Error(), "no such object") ||
			strings.Contains(err.Error(), "No such container") {
			return Container{}, false, nil
		}
		return Container{}, false, err
	}
	var raw []struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Image  string `json:"Image"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		HostConfig struct {
			Binds []string `json:"Binds"`
		} `json:"HostConfig"`
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Container{}, false, fmt.Errorf("decode docker inspect %s: %w", name, err)
	}
	if len(raw) == 0 {
		return Container{}, false, nil
	}
	c := raw[0]
	return Container{
		ID:     c.ID,
		Name:   strings.TrimPrefix(c.Name, "/"),
		Image:  c.Image,
		State:  c.State.Status,
		Binds:  c.HostConfig.Binds,
		Labels: c.Config.Labels,
	}, true, nil
}
