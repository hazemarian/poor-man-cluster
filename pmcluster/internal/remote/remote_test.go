package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/apikeys"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// fakeDaemon is a minimal httptest server emulating the pmcluster daemon
// /api/* routes. Handlers are added by the caller.
func fakeDaemon(t *testing.T, h http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(h)
	c := New(srv.URL, "tok", 0)
	return srv, c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestRemoteConfigs(t *testing.T) {
	srv, c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/configs":
			writeJSON(w, http.StatusOK, map[string]any{"configs": []map[string]any{
				{"id": 2, "scope": "service", "name": "app_env", "kind": "env", "version": "v0.2.30", "hash": "h2", "created_at": 65, "updated_at": 66},
				{"id": 9, "scope": "cluster", "name": "traefik-dynamic", "kind": "template", "version": "v0.2.40", "hash": "h9", "created_at": 65, "updated_at": 66, "rendered": true},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/configs":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			writeJSON(w, http.StatusCreated, map[string]any{"id": 10, "scope": in["scope"], "name": in["name"], "kind": in["kind"], "hash": "newhash"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/configs/app_env":
			writeJSON(w, http.StatusOK, map[string]any{"id": 2, "scope": "service", "name": "app_env", "kind": "env", "version": "v0.2.30", "hash": "h2", "content": "DATABASE_URL=postgres://x", "created_at": 65, "updated_at": 66})
		case r.Method == http.MethodPut && r.URL.Path == "/api/configs/app_env":
			writeJSON(w, http.StatusOK, map[string]any{"name": "app_env", "hash": "h3"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/configs/app_env/versions":
			writeJSON(w, http.StatusOK, map[string]any{"versions": []map[string]any{{"id": 2, "hash": "h2", "created_at": 66}, {"id": 1, "hash": "h1", "created_at": 60}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/configs/app_env/rollback":
			writeJSON(w, http.StatusOK, map[string]any{"name": "app_env", "hash": "h1", "rolled_back_to": 1})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/configs/app_env":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/cluster/rendered":
			writeJSON(w, http.StatusOK, map[string]any{"configs": []map[string]any{{"name": "traefik-dynamic", "content": "tls:\n  certificates: []"}}})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	defer srv.Close()

	svc := NewConfigs(c)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "service", "", "new_cfg", "env", "x=1", "v0.2.30"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rows, err := svc.List(ctx, "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 || rows[0].Name != "app_env" {
		t.Fatalf("List rows = %+v, want 2 starting with app_env", rows)
	}

	row, err := svc.Get(ctx, "app_env")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Content != "DATABASE_URL=postgres://x" {
		t.Fatalf("Get content = %q", row.Content)
	}

	if _, err := svc.Update(ctx, "app_env", "newval", "v0.2.30"); err != nil {
		t.Fatalf("Update: %v", err)
	}

	vers, err := svc.ListVersions(ctx, "app_env")
	if err != nil || len(vers) != 2 || vers[0].ID != 2 {
		t.Fatalf("ListVersions = %+v, err %v", vers, err)
	}

	if _, err := svc.Rollback(ctx, "app_env", 1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if err := svc.Delete(ctx, "app_env"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	rendered, err := svc.ListRendered(ctx)
	if err != nil || len(rendered) != 1 || rendered[0].Name != "traefik-dynamic" {
		t.Fatalf("ListRendered = %+v, err %v", rendered, err)
	}
	if !strings.Contains(rendered[0].Rendered, "certificates: []") {
		t.Fatalf("ListRendered content = %q", rendered[0].Rendered)
	}
}

func TestRemoteSecrets(t *testing.T) {
	srv, c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/secrets":
			writeJSON(w, http.StatusCreated, map[string]any{"id": 7, "scope": "service", "name": "db_pass", "hash": "a1b2c3"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/secrets":
			writeJSON(w, http.StatusOK, map[string]any{"secrets": []map[string]any{
				{"id": 6, "scope": "cluster", "name": "site_cert", "hash": "abc123", "created_at": 60},
				{"id": 7, "scope": "service", "name": "db_pass", "hash": "a1b2c3", "created_at": 61},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/secrets/db_pass/value":
			writeJSON(w, http.StatusOK, map[string]any{"name": "db_pass", "value": "s3cr3t-value"})
		case r.Method == http.MethodPut && r.URL.Path == "/api/secrets/db_pass":
			writeJSON(w, http.StatusOK, map[string]any{"name": "db_pass", "hash": "z9"})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/secrets/db_pass":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	defer srv.Close()

	svc := NewSecrets(c)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "service", "", "db_pass", "s3cr3t-value"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	row, err := svc.Get(ctx, "db_pass")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Name != "db_pass" || row.Hash != "a1b2c3" {
		t.Fatalf("Get = %+v", row)
	}

	val, err := svc.Reveal(ctx, "db_pass")
	if err != nil || val != "s3cr3t-value" {
		t.Fatalf("Reveal = %q, err %v", val, err)
	}

	if err := svc.Update(ctx, "db_pass", "newvalue"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := svc.Delete(ctx, "db_pass"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestRemoteStacksAndDeploy(t *testing.T) {
	srv, c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks":
			writeJSON(w, http.StatusOK, map[string]any{"stacks": []map[string]any{
				{"name": "demo", "current_revision": 1000, "repo_url": "https://github.com/x/y", "created_at": 60, "updated_at": 70},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/demo":
			writeJSON(w, http.StatusOK, map[string]any{
				"stack":     map[string]any{"name": "demo", "current_revision": 1000, "repo_url": "", "created_at": 60, "updated_at": 70},
				"revisions": []map[string]any{{"revision": 1000, "created_at": 70}, {"revision": 999, "created_at": 60}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/stacks":
			writeJSON(w, http.StatusOK, map[string]any{"stack": "demo", "revision": 1001})
		case r.Method == http.MethodPost && r.URL.Path == "/api/stacks/demo/rollback":
			writeJSON(w, http.StatusOK, map[string]any{"stack": "demo", "new_revision": 1002, "rolled_back_to": 999})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/stacks/demo":
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	defer srv.Close()

	ctx := context.Background()

	ss, err := NewStacks(c).List(ctx)
	if err != nil || len(ss) != 1 || ss[0].Name != "demo" {
		t.Fatalf("List = %+v, err %v", ss, err)
	}
	if ss[0].RepoURL != "https://github.com/x/y" {
		t.Fatalf("List repo = %+v", ss[0].RepoURL)
	}

	s, err := NewStacks(c).Get(ctx, "demo")
	if err != nil || s.CurrentRevision != 1000 {
		t.Fatalf("Get = %+v, err %v", s, err)
	}

	revs, err := NewStacks(c).Revisions(ctx, "demo", 20)
	if err != nil || len(revs) != 2 || revs[0].Revision != 1000 {
		t.Fatalf("Revisions = %+v, err %v", revs, err)
	}

	res, err := NewDeploy(c).Deploy(ctx, stacks.Payload{AppName: "demo", Manifest: "app: demo"})
	if err != nil || res.StackName != "demo" || res.Revision != 1001 {
		t.Fatalf("Deploy = %+v, err %v", res, err)
	}

	res, err = NewDeploy(c).Rollback(ctx, "demo", 999)
	if err != nil || res.Revision != 1002 {
		t.Fatalf("Rollback = %+v, err %v", res, err)
	}

	if err := NewDeploy(c).Undeploy(ctx, "demo"); err != nil {
		t.Fatalf("Undeploy: %v", err)
	}
}

func TestRemoteErrorMapping(t *testing.T) {
	srv, c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/configs/missing":
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "config not found"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/secrets":
			writeJSON(w, http.StatusConflict, map[string]any{"error": "secret already exists"})
		case r.URL.Path == "/api/stacks/ghost":
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "stack not found"})
		case r.URL.Path == "/api/api_keys/1":
			writeJSON(w, http.StatusConflict, map[string]any{"error": "the 'edge' user is required by the operator console and cannot be removed"})
		case r.URL.Path == "/api/webhooks/ghost":
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "webhook source not found"})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	defer srv.Close()

	ctx := context.Background()

	_, err := NewConfigs(c).Get(ctx, "missing")
	if !isErr(err, store.ErrConfigNotFound) {
		t.Fatalf("config 404 = %v, want ErrConfigNotFound", err)
	}

	_, err = NewSecrets(c).Create(ctx, "service", "", "dup", "x")
	if !isErr(err, store.ErrSecretExists) {
		t.Fatalf("secret 409 = %v, want ErrSecretExists", err)
	}

	_, err = NewStacks(c).Get(ctx, "ghost")
	if !isErr(err, store.ErrStackNotFound) {
		t.Fatalf("stack 404 = %v, want ErrStackNotFound", err)
	}

	err = NewAPIKeys(c).Delete(ctx, 1)
	if !isErr(err, apikeys.ErrEdgeUserProtected) {
		t.Fatalf("edge delete = %v, want ErrEdgeUserProtected", err)
	}

	err = NewWebhooks(c).Delete(ctx, "ghost")
	if !isErr(err, store.ErrWebhookSourceNotFound) {
		t.Fatalf("webhook 404 = %v, want ErrWebhookSourceNotFound", err)
	}
}

func isErr(err, target error) bool {
	return err != nil && strings.Contains(err.Error(), target.Error())
}

func TestRemoteServices(t *testing.T) {
	srv, c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/services":
			writeJSON(w, http.StatusOK, map[string]any{"services": []map[string]any{
				{"name": "demo_web", "stack": "demo", "replicas": 2, "desired": 2, "image": "img:1", "mode": "replicated", "updated": 1700000000},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/services/demo":
			writeJSON(w, http.StatusOK, map[string]any{"services": []map[string]any{
				{"name": "demo_web", "stack": "demo", "replicas": 2, "desired": 2, "image": "img:1", "mode": "replicated", "updated": 1700000000},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/services/demo/web/tasks":
			writeJSON(w, http.StatusOK, map[string]any{"tasks": []map[string]any{
				{"task_id": "t1", "node": "mgr", "slot": 1, "state": "running", "error": "", "started_at": 1700000000, "finished_at": 0},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/services/demo/web/logs":
			writeJSON(w, http.StatusOK, map[string]any{"logs": []map[string]any{
				{"stream": "stdout", "line": "hello"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/services/demo/web/restart":
			writeJSON(w, http.StatusOK, map[string]any{"service": "demo_web", "restarted": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/services/demo/web/exec":
			var in struct {
				Argv []string `json:"argv"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if len(in.Argv) == 0 {
				http.Error(w, "argv: required", http.StatusBadRequest)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"service": "demo_web", "exit_code": 0, "stdout": "root", "stderr": ""})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	defer srv.Close()

	a := NewServices(c)

	list, err := a.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "demo_web" || list[0].Stack != "demo" {
		t.Errorf("List = %+v", list)
	}

	demo, err := a.List(context.Background(), "demo")
	if err != nil || len(demo) != 1 {
		t.Errorf("List(demo) = %+v, %v", demo, err)
	}

	tasks, err := a.Tasks(context.Background(), "demo", "web")
	if err != nil || len(tasks) != 1 || tasks[0].State != "running" {
		t.Errorf("Tasks = %+v, %v", tasks, err)
	}

	logs, err := a.Logs(context.Background(), "demo", "web", 50)
	if err != nil || len(logs) != 1 || logs[0].Line != "hello" {
		t.Errorf("Logs = %+v, %v", logs, err)
	}

	if err := a.Restart(context.Background(), "demo", "web"); err != nil {
		t.Errorf("Restart: %v", err)
	}

	res, err := a.Exec(context.Background(), "demo", "web", []string{"whoami"})
	if err != nil || res.Stdout != "root" || res.ExitCode != 0 {
		t.Errorf("Exec = %+v, %v", res, err)
	}

	_, err = a.Exec(context.Background(), "demo", "web", nil)
	if err == nil || !strings.Contains(err.Error(), "argv") {
		t.Errorf("Exec(nil argv) err = %v, want argv error", err)
	}
}
