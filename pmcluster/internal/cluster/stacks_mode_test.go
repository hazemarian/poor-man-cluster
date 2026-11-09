package cluster

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// modeRunner stands in for the docker CLI. It tracks each service's live mode
// (absent = the service does not exist yet) and records every invocation, so a
// test can assert both what the deployer decided and the order it did it in.
type modeRunner struct {
	modes  map[string]string // full name → "global" | "replicated"
	rmErr  error
	calls  []string
	output string
}

func (r *modeRunner) run(_ context.Context, _ []byte, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	switch {
	case len(args) >= 2 && args[0] == "service" && args[1] == "inspect":
		name := args[len(args)-1]
		mode, ok := r.modes[name]
		if !ok {
			return "Error: no such service: " + name, errors.New("exit status 1")
		}
		return mode, nil
	case len(args) >= 2 && args[0] == "service" && args[1] == "rm":
		if r.rmErr != nil {
			return "Error response from daemon: rpc error: service is in use", r.rmErr
		}
		delete(r.modes, args[len(args)-1])
		return args[len(args)-1], nil
	case len(args) >= 2 && args[0] == "stack" && args[1] == "deploy":
		return r.output, nil
	}
	return "", nil
}

func (r *modeRunner) countCall(prefix string) int {
	n := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (r *modeRunner) indexOf(prefix string) int {
	for i, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// modeCompose renders a minimal stack compose: name → deploy mode ("" means the
// replicated default, i.e. no mode key at all — what the writer emits).
func modeCompose(services map[string]string) []byte {
	var b strings.Builder
	b.WriteString("services:\n")
	for name, mode := range services {
		b.WriteString("  " + name + ":\n    image: example/" + name + ":2\n")
		if mode != "" {
			b.WriteString("    deploy:\n      mode: " + mode + "\n")
		} else {
			b.WriteString("    deploy:\n      replicas: 1\n")
		}
	}
	return []byte(b.String())
}

// TestDeployStack_RecreatesOnModeChange pins the update Swarm cannot do in
// place: a running replicated service rendered as global must be REMOVED once
// and then deployed, so `docker stack deploy` never issues the mode change that
// it answers with "Unimplemented: service mode change is not allowed".
func TestDeployStack_RecreatesOnModeChange(t *testing.T) {
	r := &modeRunner{modes: map[string]string{"demo_agent": "replicated"}}
	var out bytes.Buffer
	d := &dockerCLIDeployer{stdout: &out, stderr: &out, run: r.run}

	if err := d.deployStack(context.Background(), "demo",
		modeCompose(map[string]string{"agent": "global", "web": ""})); err != nil {
		t.Fatalf("deployStack: %v", err)
	}

	if got := r.countCall("service rm demo_agent"); got != 1 {
		t.Fatalf("service rm demo_agent called %d times, want exactly 1\ncalls: %v", got, r.calls)
	}
	if got := r.countCall("service rm demo_web"); got != 0 {
		t.Errorf("a service whose mode matches must not be removed, got %d rm calls\ncalls: %v", got, r.calls)
	}
	rmAt := r.indexOf("service rm demo_agent")
	deployAt := r.indexOf("stack deploy")
	if rmAt < 0 || deployAt < 0 || rmAt > deployAt {
		t.Fatalf("rm must happen BEFORE the deploy (rm at %d, deploy at %d)\ncalls: %v", rmAt, deployAt, r.calls)
	}
	if msg := out.String(); !strings.Contains(msg, "demo_agent") || !strings.Contains(msg, "replicated → global") {
		t.Errorf("the recreate must be logged with the service name and mode pair, got %q", msg)
	}
}

// TestDeployStack_KeepsServicesWithIdenticalMode: the normal update path for a
// service that is already the mode the compose asks for. Removing it would
// restart every task of the stack on an otherwise no-op deploy.
func TestDeployStack_KeepsServicesWithIdenticalMode(t *testing.T) {
	r := &modeRunner{modes: map[string]string{"demo_agent": "global", "demo_web": "replicated"}}
	d := &dockerCLIDeployer{run: r.run}

	if err := d.deployStack(context.Background(), "demo",
		modeCompose(map[string]string{"agent": "global", "web": ""})); err != nil {
		t.Fatalf("deployStack: %v", err)
	}
	if got := r.countCall("service rm"); got != 0 {
		t.Fatalf("identical modes must not remove anything, got %d rm calls\ncalls: %v", got, r.calls)
	}
	if r.indexOf("stack deploy") < 0 {
		t.Errorf("the deploy must still run, calls: %v", r.calls)
	}
}

// TestDeployStack_NonModeChangeNeverRemoves: a plain spec change (a new image)
// is exactly what `docker service update` can do. Mode reconciliation must stay
// out of the way.
func TestDeployStack_NonModeChangeNeverRemoves(t *testing.T) {
	r := &modeRunner{modes: map[string]string{"demo_web": "replicated"}}
	d := &dockerCLIDeployer{run: r.run}

	compose := []byte(`services:
  web:
    image: example/web:3
    deploy:
      replicas: 2
      update_config:
        parallelism: 1
`)
	if err := d.deployStack(context.Background(), "demo", compose); err != nil {
		t.Fatalf("deployStack: %v", err)
	}
	if got := r.countCall("service rm"); got != 0 {
		t.Fatalf("a non-mode change must never remove a service, got %d rm calls\ncalls: %v", got, r.calls)
	}
}

// TestDeployStack_RemoveFailureWarnsAndDeploys: the recreate is best-effort. A
// failed remove is an apply warning — the warningSink records any "⚠" line —
// and the deploy still runs, so one unremovable service cannot abort the stack.
func TestDeployStack_RemoveFailureWarnsAndDeploys(t *testing.T) {
	r := &modeRunner{
		modes: map[string]string{"demo_agent": "replicated"},
		rmErr: errors.New("exit status 1"),
	}
	var out bytes.Buffer
	d := &dockerCLIDeployer{stdout: &out, stderr: &out, run: r.run}

	if err := d.deployStack(context.Background(), "demo",
		modeCompose(map[string]string{"agent": "global"})); err != nil {
		t.Fatalf("a failed recreate must not be fatal, got %v", err)
	}
	if r.indexOf("stack deploy") < 0 {
		t.Fatalf("the deploy must continue after a failed remove, calls: %v", r.calls)
	}
	msg := out.String()
	if !strings.Contains(msg, "⚠") {
		t.Fatalf("a failed recreate must be an apply warning,\noutput: %q", msg)
	}
	if !strings.Contains(msg, "demo_agent") || !strings.Contains(msg, "mode change replicated → global") {
		t.Errorf("the warning must name the service and the mode pair, got %q", msg)
	}
}

// TestRenderedServiceModes: parse-level contract. An absent deploy block, an
// explicit replicas count and an update_config are all replicated; only an
// explicit mode: global is global.
func TestRenderedServiceModes(t *testing.T) {
	compose := []byte(`services:
  alpha:
    image: a
  beta:
    image: b
    deploy:
      replicas: 3
  gamma:
    image: c
    deploy:
      mode: global
`)
	got, err := renderedServiceModes("demo", compose)
	if err != nil {
		t.Fatalf("renderedServiceModes: %v", err)
	}
	want := []composeServiceMode{
		{FullName: "demo_alpha", Mode: "replicated"},
		{FullName: "demo_beta", Mode: "replicated"},
		{FullName: "demo_gamma", Mode: "global"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d services, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("service %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
