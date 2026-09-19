package services

import (
	"context"
	"errors"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
)

// stubDocker embeds the runtime.Client interface and overrides the service-ops
// methods so the services local adapter can be tested without Docker.
type stubDocker struct {
	runtime.Client
	services      map[string]runtime.ServiceInspectResult
	list          []runtime.Service
	tasks         map[string][]runtime.ServiceTask
	logs          map[string][]runtime.LogLine
	restartCalled []string
	execResult    *runtime.ExecResult
	execErr       error
}

func (s *stubDocker) ServiceList(context.Context) ([]runtime.Service, error) { return s.list, nil }

func (s *stubDocker) ServiceInspect(_ context.Context, name string) (runtime.ServiceInspectResult, error) {
	svc, ok := s.services[name]
	if !ok {
		return runtime.ServiceInspectResult{}, errors.New("service not found")
	}
	return svc, nil
}

func (s *stubDocker) ServiceTasks(_ context.Context, id string) ([]runtime.ServiceTask, error) {
	for _, svc := range s.services {
		if svc.ID == id {
			return s.tasks[svc.Name], nil
		}
	}
	return nil, errors.New("service not found")
}

func (s *stubDocker) ServiceLogs(_ context.Context, id string, _ int) ([]runtime.LogLine, error) {
	for _, svc := range s.services {
		if svc.ID == id {
			return s.logs[svc.Name], nil
		}
	}
	return nil, errors.New("service not found")
}

func (s *stubDocker) ServiceRestart(_ context.Context, id string) error {
	for _, svc := range s.services {
		if svc.ID == id {
			s.restartCalled = append(s.restartCalled, svc.Name)
			return nil
		}
	}
	return errors.New("service not found")
}

func (s *stubDocker) ServiceExec(_ context.Context, id string, _ []string) (*runtime.ExecResult, error) {
	for _, svc := range s.services {
		if svc.ID == id {
			if s.execErr != nil {
				return nil, s.execErr
			}
			return s.execResult, nil
		}
	}
	return nil, errors.New("service not found")
}

func newStub() *stubDocker {
	s := &stubDocker{
		services: map[string]runtime.ServiceInspectResult{},
		tasks:    map[string][]runtime.ServiceTask{},
		logs:     map[string][]runtime.LogLine{},
		list: []runtime.Service{
			{Name: "demo_web", Stack: "demo", Replicas: 2, Desired: 2, Image: "ghcr.io/nextrum-sy/demo:1.0", Mode: "replicated"},
			{Name: "demo_db", Stack: "demo", Replicas: 1, Desired: 1, Mode: "replicated"},
			{Name: "infra_traefik", Stack: "infra", Replicas: 1, Desired: 1, Mode: "global"},
			{Name: "external", Stack: "", Replicas: 1, Desired: 1},
		},
	}
	s.services["demo_web"] = runtime.ServiceInspectResult{
		ID: "svc-web", Name: "demo_web", Labels: map[string]string{runtime.StackNamespaceLabel: "demo"},
	}
	s.services["demo_db"] = runtime.ServiceInspectResult{
		ID: "svc-db", Name: "demo_db", Labels: map[string]string{runtime.StackNamespaceLabel: "demo"},
	}
	s.tasks["demo_web"] = []runtime.ServiceTask{{TaskID: "t1", State: "running", Node: "mgr"}}
	s.logs["demo_web"] = []runtime.LogLine{{Stream: "stdout", Line: "hi"}}
	return s
}

func TestLocalList_AllAndFiltered(t *testing.T) {
	stub := newStub()
	l := Local{Docker: stub}

	all, err := l.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("List() returned %d, want 4", len(all))
	}

	demo, err := l.List(context.Background(), "demo")
	if err != nil {
		t.Fatalf("List(demo): %v", err)
	}
	if len(demo) != 2 {
		t.Fatalf("List(demo) returned %d, want 2", len(demo))
	}
	if demo[0].Name != "demo_web" {
		t.Errorf("first demo service = %q, want demo_web", demo[0].Name)
	}
}

func TestLocalList_RejectsBadStack(t *testing.T) {
	l := Local{Docker: newStub()}
	if _, err := l.List(context.Background(), "Bad_Name"); err == nil {
		t.Fatal("expected error for invalid stack name")
	}
}

func TestLocalTasks_ResolvesStackService(t *testing.T) {
	l := Local{Docker: newStub()}
	tasks, err := l.Tasks(context.Background(), "demo", "web")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].TaskID != "t1" {
		t.Errorf("tasks = %+v, want [t1]", tasks)
	}
}

func TestLocalTasks_CrossStackRejected(t *testing.T) {
	l := Local{Docker: newStub()}
	// demo_web carries stack=demo; asking for stack "infra" must refuse.
	_, err := l.Tasks(context.Background(), "infra", "web")
	if err == nil {
		t.Fatal("expected cross-stack targeting to be rejected")
	}
}

func TestLocalLogs(t *testing.T) {
	l := Local{Docker: newStub()}
	lines, err := l.Logs(context.Background(), "demo", "web", 10)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(lines) != 1 || lines[0].Line != "hi" {
		t.Errorf("lines = %+v, want [hi]", lines)
	}
}

func TestLocalRestart(t *testing.T) {
	stub := newStub()
	l := Local{Docker: stub}
	if err := l.Restart(context.Background(), "demo", "web"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if len(stub.restartCalled) != 1 || stub.restartCalled[0] != "demo_web" {
		t.Errorf("restartCalled = %v, want [demo_web]", stub.restartCalled)
	}
}

func TestLocalExec(t *testing.T) {
	stub := newStub()
	stub.execResult = &runtime.ExecResult{ExitCode: 0, Stdout: "root"}
	l := Local{Docker: stub}
	res, err := l.Exec(context.Background(), "demo", "web", []string{"whoami"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "root" {
		t.Errorf("res = %+v, want exit 0 stdout root", res)
	}
}

func TestLocalExec_EmptyArgv(t *testing.T) {
	l := Local{Docker: newStub()}
	_, err := l.Exec(context.Background(), "demo", "web", nil)
	if err == nil {
		t.Fatal("expected error for empty argv")
	}
}
