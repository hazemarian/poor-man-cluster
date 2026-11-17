package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// fakeShell is the engine slice the on-demand shell needs, with the calls it makes recorded.
type fakeShell struct {
	node      string
	nodeErr   error
	networks  []string
	netErr    error
	createErr error
	waitErr   error
	spec      runtime.ShellServiceSpec
	created   int
	removed   []string
}

func (f *fakeShell) ShellCreate(_ context.Context, spec runtime.ShellServiceSpec) (string, error) {
	f.created++
	f.spec = spec
	if f.createErr != nil {
		return "", f.createErr
	}
	return "svc-id", nil
}

func (f *fakeShell) ShellContainerOn(_ context.Context, _ string, _ string) (string, error) {
	if f.waitErr != nil {
		return "", f.waitErr
	}
	return "container-id", nil
}

func (f *fakeShell) ShellRemove(_ context.Context, serviceID string) error {
	f.removed = append(f.removed, serviceID)
	return nil
}

func (f *fakeShell) StackNetworks(_ context.Context, _ string) ([]string, error) {
	return f.networks, f.netErr
}

func (f *fakeShell) SelfHostname(_ context.Context) (string, error) {
	return f.node, f.nodeErr
}

// shellClients is a runtime.Client whose ShellRuntime half is the fake above.
type shellClients struct {
	runtime.Client
	shell *fakeShell
}

func (c shellClients) ShellCreate(ctx context.Context, spec runtime.ShellServiceSpec) (string, error) {
	return c.shell.ShellCreate(ctx, spec)
}
func (c shellClients) ShellContainerOn(ctx context.Context, id, node string) (string, error) {
	return c.shell.ShellContainerOn(ctx, id, node)
}
func (c shellClients) ShellRemove(ctx context.Context, id string) error {
	return c.shell.ShellRemove(ctx, id)
}
func (c shellClients) StackNetworks(ctx context.Context, stack string) ([]string, error) {
	return c.shell.StackNetworks(ctx, stack)
}
func (c shellClients) SelfHostname(ctx context.Context) (string, error) {
	return c.shell.SelfHostname(ctx)
}

// TestOpenShell_PinsTheSessionToThisNode is the property the whole design rests on: the container
// must run where the daemon can exec into it, so the node is not a preference — it is the answer.
func TestOpenShell_PinsTheSessionToThisNode(t *testing.T) {
	sh := &fakeShell{node: "nxt-sw-5-m", networks: []string{"net-a", "net-b"}}
	svc := Local{Docker: shellClients{shell: sh}}

	sess, err := svc.OpenShell(context.Background(), ShellRequest{
		Stack:       "aarma",
		Image:       "ghcr.io/hazemarian/pmcluster-shell:v1",
		SecretNames: []string{"db_password_abc"},
		ConfigNames: []string{"app_yaml_def"},
	})
	if err != nil {
		t.Fatalf("OpenShell: %v", err)
	}
	if sh.spec.Node != "nxt-sw-5-m" {
		t.Errorf("shell scheduled on %q, want this node — an exec must be local", sh.spec.Node)
	}
	if len(sh.spec.Networks) != 2 || len(sh.spec.SecretNames) != 1 || len(sh.spec.ConfigNames) != 1 {
		t.Errorf("shell spec lost its stack's context: %+v", sh.spec)
	}
	if sh.spec.Life != runtime.ShellMaxLife {
		t.Errorf("shell life = %v, want %v", sh.spec.Life, runtime.ShellMaxLife)
	}
	if sess.ContainerID != "container-id" || sess.ServiceID != "svc-id" {
		t.Errorf("session = %+v", sess)
	}
}

// TestOpenShell_RefusesAStackWithNothingToReach: a shell with no network can reach no service, so
// creating it would hand the operator a prompt that cannot answer the question they opened it for.
func TestOpenShell_RefusesAStackWithNothingToReach(t *testing.T) {
	sh := &fakeShell{node: "n", networks: nil}
	svc := Local{Docker: shellClients{shell: sh}}
	_, err := svc.OpenShell(context.Background(), ShellRequest{Stack: "empty", Image: "img"})
	if err == nil {
		t.Fatal("a stack with no networks produced a shell")
	}
	if !strings.Contains(err.Error(), "nothing to reach") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
	if sh.created != 0 {
		t.Error("a container was created for a stack that cannot be reached")
	}
}

// TestOpenShell_CleansUpWhenTheContainerNeverStarts: a wait failure must not leave a service behind.
func TestOpenShell_CleansUpWhenTheContainerNeverStarts(t *testing.T) {
	sh := &fakeShell{
		node:     "n",
		networks: []string{"net-a"},
		waitErr:  errors.New("image pull failed"),
	}
	svc := Local{Docker: shellClients{shell: sh}}
	if _, err := svc.OpenShell(context.Background(), ShellRequest{Stack: "aarma", Image: "img"}); err == nil {
		t.Fatal("OpenShell succeeded although the container never started")
	}
	if len(sh.removed) != 1 || sh.removed[0] != "svc-id" {
		t.Errorf("the half-created service was not removed: %v", sh.removed)
	}
}

// TestCloseShell_RemovesTheService: the operator's rule is that no shell outlives its socket.
func TestCloseShell_RemovesTheService(t *testing.T) {
	sh := &fakeShell{node: "n", networks: []string{"net-a"}}
	svc := Local{Docker: shellClients{shell: sh}}
	if err := svc.CloseShell(context.Background(), "svc-id"); err != nil {
		t.Fatalf("CloseShell: %v", err)
	}
	if len(sh.removed) != 1 {
		t.Fatalf("CloseShell removed %v, want the service", sh.removed)
	}
	// An empty id is a session nobody created: not an error, just nothing to do.
	if err := svc.CloseShell(context.Background(), ""); err != nil {
		t.Errorf("CloseShell(\"\") = %v, want nil", err)
	}
}

// TestOpenShell_ReportsAnEngineWithoutShellSupport: a worker's runtime may not implement the shell
// slice; the operator gets a sentence, not a panic.
func TestOpenShell_ReportsAnEngineWithoutShellSupport(t *testing.T) {
	svc := Local{Docker: nilClient{}}
	if _, err := svc.OpenShell(context.Background(), ShellRequest{Stack: "aarma", Image: "img"}); err == nil {
		t.Fatal("a client without shell support produced a session")
	} else if !strings.Contains(err.Error(), "cannot create a shell session") {
		t.Errorf("unclear refusal: %v", err)
	}
}

type nilClient struct{ runtime.Client }
