package services

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// fakeService implements Service with canned data and error injection.
type fakeService struct {
	listFn    func(ctx context.Context, stack string) ([]ServiceSummary, error)
	tasksFn   func(ctx context.Context, stack, service string) ([]TaskRun, error)
	logsFn    func(ctx context.Context, stack, service string, tail int) ([]LogLine, error)
	restartFn func(ctx context.Context, stack, service string) error
	execFn    func(ctx context.Context, stack, service string, argv []string) (*ExecResult, error)
}

func (f *fakeService) List(ctx context.Context, stack string) ([]ServiceSummary, error) {
	return f.listFn(ctx, stack)
}
func (f *fakeService) Tasks(ctx context.Context, stack, service string) ([]TaskRun, error) {
	return f.tasksFn(ctx, stack, service)
}
func (f *fakeService) Logs(ctx context.Context, stack, service string, tail int) ([]LogLine, error) {
	return f.logsFn(ctx, stack, service, tail)
}
func (f *fakeService) Restart(ctx context.Context, stack, service string) error {
	return f.restartFn(ctx, stack, service)
}
func (f *fakeService) Exec(ctx context.Context, stack, service string, argv []string) (*ExecResult, error) {
	return f.execFn(ctx, stack, service, argv)
}

func newServicesMux(svc Service) chi.Router {
	r := chi.NewRouter()
	(&HTTP{Svc: svc}).Mount(r)
	return r
}

func doJSON(t *testing.T, mux http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	if len(rec.Body.Bytes()) > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func TestHTTP_ListAndListStack(t *testing.T) {
	svcs := []ServiceSummary{
		{Name: "demo_web", Stack: "demo", Replicas: 2, Desired: 2, Image: "nginx", Mode: "replicated"},
		{Name: "infra_traefik", Stack: "infra", Replicas: 1, Desired: 1, Image: "traefik", Mode: "global"},
	}
	mux := newServicesMux(&fakeService{listFn: func(_ context.Context, stack string) ([]ServiceSummary, error) {
		if stack != "" {
			var filtered []ServiceSummary
			for _, s := range svcs {
				if s.Stack == stack {
					filtered = append(filtered, s)
				}
			}
			return filtered, nil
		}
		return svcs, nil
	}})

	// All services.
	rec, body := doJSON(t, mux, "GET", "/services", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d, body=%s", rec.Code, rec.Body.String())
	}
	rows, ok := body["services"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("list services = %v, want 2 rows", body["services"])
	}
	first := rows[0].(map[string]any)
	if first["name"] != "demo_web" || first["replicas"] != float64(2) || first["mode"] != "replicated" {
		t.Errorf("first service = %v, want name/replicas/mode", first)
	}

	// Stack-scoped.
	rec, body = doJSON(t, mux, "GET", "/services/infra", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("listStack code = %d, body=%s", rec.Code, rec.Body.String())
	}
	rows = body["services"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["name"] != "infra_traefik" {
		t.Errorf("stack-scoped rows = %v, want only infra_traefik", rows)
	}
	if body["stack"] != "infra" {
		t.Errorf("stack field = %v, want infra", body["stack"])
	}

	// List error → 500.
	muxErr := newServicesMux(&fakeService{listFn: func(context.Context, string) ([]ServiceSummary, error) {
		return nil, errors.New("boom")
	}})
	rec, _ = doJSON(t, muxErr, "GET", "/services", "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list error code = %d, want 500", rec.Code)
	}
}

func TestHTTP_TasksAndLogs(t *testing.T) {
	mux := newServicesMux(&fakeService{
		tasksFn: func(_ context.Context, stack, service string) ([]TaskRun, error) {
			if stack != "demo" || service != "web" {
				return nil, errors.New("unknown service")
			}
			return []TaskRun{{TaskID: "t1", Node: "node-a", State: "running", StartedAt: 100}}, nil
		},
		logsFn: func(_ context.Context, stack, service string, tail int) ([]LogLine, error) {
			if stack != "demo" || service != "web" {
				return nil, errors.New("unknown service")
			}
			return []LogLine{{Stream: "stdout", Line: "listening on :8080"}}, nil
		},
	})

	rec, body := doJSON(t, mux, "GET", "/services/demo/web/tasks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tasks code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body["service"] != "demo_web" {
		t.Errorf("tasks service = %v, want demo_web", body["service"])
	}
	tasks := body["tasks"].([]any)
	t0 := tasks[0].(map[string]any)
	if t0["task_id"] != "t1" || t0["node"] != "node-a" || t0["state"] != "running" {
		t.Errorf("task = %v, want t1/node-a/running", t0)
	}

	rec, body = doJSON(t, mux, "GET", "/services/demo/web/logs?tail=50", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs code = %d, body=%s", rec.Code, rec.Body.String())
	}
	logs := body["logs"].([]any)
	l0 := logs[0].(map[string]any)
	if l0["stream"] != "stdout" || l0["line"] != "listening on :8080" {
		t.Errorf("log line = %v, want stdout/listening", l0)
	}

	// Bad tail → 400.
	rec, _ = doJSON(t, mux, "GET", "/services/demo/web/logs?tail=abc", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad tail code = %d, want 400", rec.Code)
	}

	// Unknown service → 400.
	rec, _ = doJSON(t, mux, "GET", "/services/ghost/x/tasks", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown service code = %d, want 400", rec.Code)
	}
}

func TestHTTP_RestartAndExec(t *testing.T) {
	restarted := false
	mux := newServicesMux(&fakeService{
		restartFn: func(_ context.Context, stack, service string) error {
			if stack != "demo" || service != "web" {
				return errors.New("unknown service")
			}
			restarted = true
			return nil
		},
		execFn: func(_ context.Context, stack, service string, argv []string) (*ExecResult, error) {
			if stack != "demo" || service != "web" {
				return nil, errors.New("unknown service")
			}
			return &ExecResult{ExitCode: 0, Stdout: "root\n", Stderr: ""}, nil
		},
	})

	rec, body := doJSON(t, mux, "POST", "/services/demo/web/restart", "")
	if rec.Code != http.StatusOK || !restarted {
		t.Fatalf("restart code = %d restarted=%v body=%s", rec.Code, restarted, rec.Body.String())
	}
	if body["restarted"] != true {
		t.Errorf("restart body = %v, want restarted:true", body)
	}

	rec, body = doJSON(t, mux, "POST", "/services/demo/web/exec", `{"argv":["whoami"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("exec code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body["exit_code"] != float64(0) || body["stdout"] != "root\n" {
		t.Errorf("exec body = %v, want exit 0 + root", body)
	}

	// Malformed exec body → 400.
	rec, _ = doJSON(t, mux, "POST", "/services/demo/web/exec", `{not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad exec body code = %d, want 400", rec.Code)
	}

	// Unknown service → 400.
	rec, _ = doJSON(t, mux, "POST", "/services/ghost/x/restart", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown restart code = %d, want 400", rec.Code)
	}
}
