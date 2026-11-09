package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// StackDeployer applies a compose file to the swarm under a given stack
// name. Production impl shells out to the `docker` CLI because the SDK
// has no high-level stack-deploy primitive. The interface is owned by the
// runtime leaf package (runtime.StackDeployer); this alias keeps every
// existing reference compiling unchanged.
type StackDeployer = runtime.StackDeployer

type dockerCLIDeployer struct {
	envExtras []string
	stdout    io.Writer
	stderr    io.Writer

	// run executes one docker command. Nil means the real CLI; tests set it
	// to drive the deployer's decisions without a daemon.
	run dockerRunner
}

// dockerRunner runs `docker <args>` (with optional stdin) and returns the
// combined output, matching runWithOutput's contract.
type dockerRunner func(ctx context.Context, stdin []byte, args ...string) (string, error)

// docker dispatches to the injected runner or the real CLI. Every docker
// invocation in this file goes through here so the mode reconciliation below
// is testable without a Docker socket — the decision it makes (remove this
// service or leave it) is what the tests pin, not the argv shape of exec.
func (d *dockerCLIDeployer) docker(ctx context.Context, stdin []byte, args ...string) (string, error) {
	if d.run != nil {
		return d.run(ctx, stdin, args...)
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return d.runWithOutput(cmd)
}

// notice writes an informational line to the run's output (the same stream
// the workflow reporter and the warning sink read).
func (d *dockerCLIDeployer) notice(format string, args ...any) {
	w := d.stdout
	if w == nil {
		w = d.stderr
	}
	if w != nil {
		fmt.Fprintf(w, format, args...)
	}
}

// NewDockerCLIDeployer streams the docker process's stdout/stderr to w
// (os.Stdout for live progress; nil to discard).  On failure the full
// captured output is attached to the error so callers (REST/webhook/CLI)
// can diagnose without SSH-ing into the host.
func NewDockerCLIDeployer(w io.Writer) StackDeployer {
	return &dockerCLIDeployer{stdout: w, stderr: w}
}

// runWithOutput captures combined stdout+stderr; always returns the
// captured output (trimmed) so callers can inspect it on both success
// and failure.
func (d *dockerCLIDeployer) runWithOutput(cmd *exec.Cmd) (string, error) {
	var buf bytes.Buffer

	if d.stdout != nil {
		cmd.Stdout = io.MultiWriter(&buf, d.stdout)
		cmd.Stderr = io.MultiWriter(&buf, d.stderr)
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	if len(d.envExtras) > 0 {
		cmd.Env = append([]string{}, d.envExtras...)
	}
	err := cmd.Run()
	return trimOutput(buf.String()), err
}

// trimOutput strips trailing whitespace so error messages aren't padded
// with blank lines.
func trimOutput(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func (d *dockerCLIDeployer) DeployStack(ctx context.Context, name string, composeYAML []byte) error {
	if err := d.deployStack(ctx, name, composeYAML); err != nil {
		return err
	}
	if err := d.pruneStackServices(ctx, name, composeYAML); err != nil {
		return err
	}
	return d.forceUpdateStackServices(ctx, name)
}

// deployStack runs `docker stack deploy` for the given compose (with the
// "update out of sequence" retry). It does NOT prune or force-update —
// those are the caller's choice (full-stack vs ordered per-level deploys).
//
// Before the deploy it recreates services whose swarm mode changed (see
// recreateModeChanges): `docker service update` cannot change a mode, and
// leaving it to `docker stack deploy` aborts the whole stack.
func (d *dockerCLIDeployer) deployStack(ctx context.Context, name string, composeYAML []byte) error {
	if err := d.recreateModeChanges(ctx, name, composeYAML); err != nil {
		return err
	}
	deploy := func() (string, error) {
		return d.docker(ctx, composeYAML, "stack", "deploy",
			"--detach=true",
			"--resolve-image=always",
			"--with-registry-auth",
			"-c", "-",
			name,
		)
	}

	out, err := deploy()
	if err != nil {
		if strings.Contains(out, "update out of sequence") {
			for i := 1; i <= forceUpdateRetries; i++ {
				select {
				case <-ctx.Done():
					return fmt.Errorf("docker stack deploy %s: %w", name, ctx.Err())
				case <-time.After(forceUpdateBackoff):
				}
				if retryOut, retryErr := deploy(); retryErr == nil {
					out, err = retryOut, nil
					break
				} else {
					out = retryOut
					err = retryErr
					if !strings.Contains(retryOut, "update out of sequence") {
						break
					}
				}
			}
		}
		if err != nil {
			if out != "" {
				return fmt.Errorf("docker stack deploy %s: %s", name, out)
			}
			return fmt.Errorf("docker stack deploy %s: %w", name, err)
		}
	}
	return nil
}

// DeployStackNoPrune implements StackDeployer — the raw stack deploy with
// no reconcile (see the interface doc for why the ordered deploy needs it).
func (d *dockerCLIDeployer) DeployStackNoPrune(ctx context.Context, name string, composeYAML []byte) error {
	return d.deployStack(ctx, name, composeYAML)
}

// composeServiceMode is one rendered service's swarm mode, with the full
// stack-prefixed name the swarm knows it by.
type composeServiceMode struct {
	FullName string
	Mode     string // "global" or "replicated"
}

// renderedServiceModes lists the mode each service in composeYAML asks for.
//
// Only deploy.mode == global is special: everything else — no deploy block,
// an explicit replicas count, an update_config — is a replicated service, which
// is what Swarm reports for it. Names are sorted so a mode reconciliation
// touches the stack in a stable order (and the tests can assert an order).
func renderedServiceModes(stackName string, composeYAML []byte) ([]composeServiceMode, error) {
	var doc struct {
		Services map[string]struct {
			Deploy *struct {
				Mode string `json:"mode"`
			} `json:"deploy"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("parse compose for %s (service modes): %w", stackName, err)
	}
	out := make([]composeServiceMode, 0, len(doc.Services))
	for svc, spec := range doc.Services {
		mode := "replicated"
		if spec.Deploy != nil && spec.Deploy.Mode == "global" {
			mode = "global"
		}
		out = append(out, composeServiceMode{FullName: stackName + "_" + svc, Mode: mode})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	return out, nil
}

// swarmServiceMode returns the live mode of a swarm service. ok is false when
// the service does not exist yet (nothing to compare, the deploy creates it) or
// when the inspect failed for any other reason: an unreadable spec is not
// evidence of a mode change, and removing a service on a failed read would be
// destructive.
func (d *dockerCLIDeployer) swarmServiceMode(ctx context.Context, fullName string) (mode string, ok bool) {
	out, err := d.docker(ctx, nil, "service", "inspect",
		"--format", "{{if .Spec.Mode.Global}}global{{else}}replicated{{end}}",
		fullName)
	if err != nil {
		return "", false
	}
	mode = strings.TrimSpace(out)
	if mode != "global" && mode != "replicated" {
		return "", false
	}
	return mode, true
}

// recreateModeChanges removes live stack services whose swarm mode differs from
// the one the rendered compose asks for, so the deploy that follows recreates
// them.
//
// `docker service update` cannot change a service's mode. Swarm answers
// `Unimplemented: service mode change is not allowed`, and `docker stack
// deploy` — which issues that same update — aborts the WHOLE stack on it. A
// single global→replicated flip therefore left every other service of the stack
// on its old spec, and on `cluster update` left the edge console fleet-wide on
// the old image. A mode change is the one update Swarm cannot do in place, so
// the service is removed and left to the deploy to recreate.
//
// Only a mode difference triggers this: identical modes and every non-mode
// change keep the normal update path. A failed remove is a warning, not a fatal
// — the deploy still runs and reports its own error if the mode really could
// not be changed.
func (d *dockerCLIDeployer) recreateModeChanges(ctx context.Context, stackName string, composeYAML []byte) error {
	wanted, err := renderedServiceModes(stackName, composeYAML)
	if err != nil {
		return err
	}
	for _, svc := range wanted {
		live, ok := d.swarmServiceMode(ctx, svc.FullName)
		if !ok || live == svc.Mode {
			continue
		}
		d.notice("  ▶ %s: mode %s → %s; removing it so the deploy recreates it\n",
			svc.FullName, live, svc.Mode)
		if out, rmErr := d.docker(ctx, nil, "service", "rm", svc.FullName); rmErr != nil {
			d.notice("  ⚠ %s: mode change %s → %s could not be applied: %s — docker cannot change a service's mode in place, so this service keeps its old mode until it is removed by hand\n",
				svc.FullName, live, svc.Mode, firstLine(out, rmErr))
		}
	}
	return nil
}

// firstLine renders a failed docker call in one warning line: the CLI's output
// when it has one (it names the actual Swarm error), the error otherwise.
func firstLine(out string, err error) string {
	if s := strings.TrimSpace(out); s != "" {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i]
		}
		return s
	}
	return err.Error()
}

// PruneStack implements StackDeployer — drift-prune only (no deploy, no
// force-update). Called once after an ordered per-level deploy with the
// full stack compose so services dropped from the manifest are removed.
func (d *dockerCLIDeployer) PruneStack(ctx context.Context, name string, composeYAML []byte) error {
	return d.pruneStackServices(ctx, name, composeYAML)
}

// pruneStackServices removes services that belong to the stack but are no
// longer declared in the compose file. Modern `docker stack deploy` only
// adds/updates services — it never removes ones dropped from the compose — so
// a service deleted from the YAML (e.g. Portainer in v0.2.43) would otherwise
// keep running forever. This restores the legacy prune behaviour for ALL stack
// deploys (platform + app): diff the stack's live service list against the
// compose's top-level `services:` keys and `docker service rm` the leftovers.
func (d *dockerCLIDeployer) pruneStackServices(ctx context.Context, stackName string, composeYAML []byte) error {
	listOut, err := d.docker(ctx, nil, "stack", "services",
		"--format", "{{.Name}}", stackName)
	if err != nil {
		return fmt.Errorf("docker stack services %s (prune): %s", stackName, listOut)
	}
	livesvc := strings.Split(strings.TrimSpace(listOut), "\n")
	remove, err := pruneCandidates(stackName, composeYAML, livesvc)
	if err != nil {
		return err
	}
	for _, fullName := range remove {
		if rmOut, rmErr := d.docker(ctx, nil, "service", "rm", fullName); rmErr != nil {
			return fmt.Errorf("docker service rm %s: %s", fullName, rmOut)
		}
	}
	return nil
}

// pruneCandidates computes which live stack services should be removed given
// the compose's declared services: every live full name (stack_service) that
// is NOT in the compose. Safe by construction: when the compose declares no
// services (parse surprise) nothing is pruned — a running stack must never be
// wiped by a bad parse.
func pruneCandidates(stackName string, composeYAML []byte, live []string) ([]string, error) {
	var doc struct {
		Services map[string]any `json:"services"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("parse compose for %s (prune): %w", stackName, err)
	}
	if len(doc.Services) == 0 {
		return nil, nil
	}
	wanted := make(map[string]bool, len(doc.Services))
	for svc := range doc.Services {
		wanted[stackName+"_"+svc] = true
	}
	var remove []string
	for _, fullName := range live {
		fullName = strings.TrimSpace(fullName)
		if fullName == "" || wanted[fullName] {
			continue
		}
		remove = append(remove, fullName)
	}
	return remove, nil
}

// forceUpdateStackServices lists services belonging to a stack and
// force-restarts them. Replicated services get `docker service update
// --force`; run_once jobs (which become terminal after completion) get
// removed before stack deploy so they are re-created as fresh tasks.
func (d *dockerCLIDeployer) forceUpdateStackServices(ctx context.Context, stackName string) error {
	listOut, err := d.docker(ctx, nil, "stack", "services",
		"--format", "{{.Name}}", stackName)
	if err != nil {
		return fmt.Errorf("docker stack services %s: %s", stackName, listOut)
	}

	for _, fullName := range strings.Split(strings.TrimSpace(listOut), "\n") {
		fullName = strings.TrimSpace(fullName)
		if fullName == "" {
			continue
		}
		if err := d.ForceUpdateService(ctx, fullName); err != nil {
			return err
		}
	}
	return nil
}

func (d *dockerCLIDeployer) RemoveStack(ctx context.Context, name string) error {
	out, err := d.docker(ctx, nil, "stack", "rm", name)
	if err != nil {
		if out != "" {
			return fmt.Errorf("docker stack rm %s: %s", name, out)
		}
		return fmt.Errorf("docker stack rm %s: %w", name, err)
	}
	return nil
}

// forceUpdateRetries is how many times a single service's forced update is
// retried when Swarm reports "update out of sequence" — a transient error
// that occurs when `docker stack deploy` returns before the manager has
// finished reconciling the stack's service specs, so the immediate
// `docker service update --force` is racing an in-flight update.
const forceUpdateRetries = 5

// forceUpdateBackoff is the sleep between retries.
const forceUpdateBackoff = 500 * time.Millisecond

func (d *dockerCLIDeployer) ForceUpdateService(ctx context.Context, fullName string) error {
	out, err := d.docker(ctx, nil, "service", "update",
		"--force",
		"--detach=true",
		fullName,
	)
	if err != nil {

		if strings.Contains(out, "update out of sequence") {
			for i := 1; i <= forceUpdateRetries; i++ {
				select {
				case <-ctx.Done():
					return fmt.Errorf("docker service update --force %s: %w", fullName, ctx.Err())
				case <-time.After(forceUpdateBackoff):
				}
				if retryOut, retryErr := d.docker(ctx, nil, "service", "update",
					"--force", "--detach=true", fullName); retryErr == nil {
					return nil
				} else {
					out = retryOut
					err = retryErr
					if !strings.Contains(retryOut, "update out of sequence") {
						break
					}
				}
			}
		}
		if out != "" {
			return fmt.Errorf("docker service update --force %s: %s", fullName, out)
		}
		return fmt.Errorf("docker service update --force %s: %w", fullName, err)
	}
	return nil
}

func (d *dockerCLIDeployer) PruneStaleContainers(ctx context.Context, stackName string, olderThan string) error {

	listOut, err := d.docker(ctx, nil, "container", "ls", "-a",
		"--filter", "status=exited",
		"--filter", "name=^/"+stackName+"_",
		"--format", "{{.ID}}",
	)
	if err != nil {
		return fmt.Errorf("docker container ls (stale %s): %s", stackName, listOut)
	}

	dur, err := parseDuration(olderThan)
	if err != nil {
		return fmt.Errorf("prune: invalid duration %q: %w", olderThan, err)
	}

	for _, id := range strings.Split(strings.TrimSpace(listOut), "\n") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}

		finishedAt, err := d.docker(ctx, nil, "inspect",
			"--format", "{{.State.FinishedAt}}", id)
		if err != nil {
			continue
		}

		t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(finishedAt))
		if err != nil {
			continue
		}

		if time.Since(t) < dur {
			continue
		}

		_, _ = d.docker(ctx, nil, "rm", id)
	}
	return nil
}

// parseDuration converts a human-readable duration like "10m" or "1h" to
// a time.Duration.  It accepts the same suffixes as time.ParseDuration.
func parseDuration(s string) (time.Duration, error) {
	return time.ParseDuration(s)
}
