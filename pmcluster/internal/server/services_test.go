package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/services"
)

// fakeServices is a deterministic in-memory services.Service.
type fakeServices struct {
	listErr       error
	list          []services.ServiceSummary
	tasks         []services.TaskRun
	tasksErr      error
	logs          []services.LogLine
	logsErr       error
	restartCalled string
	restartErr    error
	execRes       *services.ExecResult
	execErr       error
	execArgv      []string
}

func (f *fakeServices) List(_ context.Context, stack string) ([]services.ServiceSummary, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if stack == "" {
		return f.list, nil
	}
	var out []services.ServiceSummary
	for _, s := range f.list {
		if s.Stack == stack {
			out = append(out, s)
		}
	}
	return out, nil
}
func (f *fakeServices) Tasks(_ context.Context, _, _ string) ([]services.TaskRun, error) {
	return f.tasks, f.tasksErr
}
func (f *fakeServices) Logs(_ context.Context, _, _ string, _ int) ([]services.LogLine, error) {
	return f.logs, f.logsErr
}
func (f *fakeServices) Restart(_ context.Context, stack, service string) error {
	f.restartCalled = stack + "_" + service
	return f.restartErr
}
func (f *fakeServices) Exec(_ context.Context, _, _ string, argv []string) (*services.ExecResult, error) {
	f.execArgv = argv
	return f.execRes, f.execErr
}

func newServicesServer(fs *fakeServices) *httptest.Server {
	return httptest.NewServer(New(Deps{
		Lookup:   &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Services: fs,
	}))
}

func TestServicesAPI_List(t *testing.T) {
	fs := &fakeServices{list: []services.ServiceSummary{
		{Name: "demo_web", Stack: "demo", Replicas: 2, Desired: 2, Mode: "replicated"},
		{Name: "infra_traefik", Stack: "infra", Replicas: 1, Desired: 1, Mode: "global"},
	}}
	srv := newServicesServer(fs)
	defer srv.Close()

	if resp := doJSON(t, http.MethodGet, srv.URL+"/api/services", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth GET /services = %d, want 401", resp.StatusCode)
	}
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/services", "tok", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /services = %d, want 200", resp.StatusCode)
	}
	body := decodeBody[struct {
		Services []struct {
			Name  string `json:"name"`
			Stack string `json:"stack"`
		} `json:"services"`
	}](t, resp)
	if len(body.Services) != 2 {
		t.Fatalf("got %d services, want 2", len(body.Services))
	}

	resp = doJSON(t, http.MethodGet, srv.URL+"/api/services/demo", "tok", nil)
	defer resp.Body.Close()
	body = decodeBody[struct {
		Services []struct {
			Name  string `json:"name"`
			Stack string `json:"stack"`
		} `json:"services"`
	}](t, resp)
	if len(body.Services) != 1 || body.Services[0].Name != "demo_web" {
		t.Fatalf("demo services = %+v, want [demo_web]", body.Services)
	}
}

func TestServicesAPI_TasksLogsRestartExec(t *testing.T) {
	fs := &fakeServices{
		tasks:   []services.TaskRun{{TaskID: "t1", State: "running"}},
		logs:    []services.LogLine{{Stream: "stdout", Line: "hi"}},
		execRes: &services.ExecResult{ExitCode: 0, Stdout: "root"},
	}
	srv := newServicesServer(fs)
	defer srv.Close()
	base := srv.URL + "/api/services/demo/web"

	resp := doJSON(t, http.MethodGet, base+"/tasks", "tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET tasks = %d", resp.StatusCode)
	}
	tb := decodeBody[struct {
		Tasks []map[string]any `json:"tasks"`
	}](t, resp)
	if len(tb.Tasks) != 1 {
		t.Fatalf("tasks = %+v", tb.Tasks)
	}

	resp = doJSON(t, http.MethodGet, base+"/logs?tail=50", "tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET logs = %d", resp.StatusCode)
	}
	lb := decodeBody[struct {
		Logs []map[string]any `json:"logs"`
	}](t, resp)
	if len(lb.Logs) != 1 || lb.Logs[0]["line"] != "hi" {
		t.Fatalf("logs = %+v", lb.Logs)
	}

	resp = doJSON(t, http.MethodPost, base+"/restart", "tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST restart = %d", resp.StatusCode)
	}
	if fs.restartCalled != "demo_web" {
		t.Errorf("restartCalled = %q, want demo_web", fs.restartCalled)
	}

	resp = doJSON(t, http.MethodPost, base+"/exec", "tok", map[string]any{"argv": []string{"whoami"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST exec = %d", resp.StatusCode)
	}
	eb := decodeBody[struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}](t, resp)
	if eb.ExitCode != 0 || eb.Stdout != "root" {
		t.Fatalf("exec body = %+v", eb)
	}
	if len(fs.execArgv) != 1 || fs.execArgv[0] != "whoami" {
		t.Errorf("execArgv = %v", fs.execArgv)
	}
}

func TestServicesAPI_Errors(t *testing.T) {
	fs := &fakeServices{
		listErr: errors.New("docker down"),
		execErr: errors.New("argv: at most 16 arguments"),
	}
	srv := newServicesServer(fs)
	defer srv.Close()

	resp := doJSON(t, http.MethodGet, srv.URL+"/api/services", "tok", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("list err = %d, want 500", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodPost, srv.URL+"/api/services/demo/web/exec", "tok", map[string]any{"argv": []string{"x"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("exec err = %d, want 400", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodPost, srv.URL+"/api/services/demo/web/exec", "tok", map[string]any{"argv": "not-a-list"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad exec body = %d, want 400", resp.StatusCode)
	}
}
