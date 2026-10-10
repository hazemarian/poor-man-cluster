package pmapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeDaemon is a minimal HTTP daemon speaking the pmcluster /api surface.
// Routes return canned JSON; the handler also asserts the Bearer token so
// auth plumbing is exercised on every call.
type fakeDaemon struct {
	t        *testing.T
	token    string
	requests []string // method + path log for ordering assertions
}

func (f *fakeDaemon) handler() http.Handler {
	mux := http.NewServeMux()

	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			f.t.Errorf("encode response: %v", err)
		}
	}
	writeErr := func(w http.ResponseWriter, status int, msg string) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error": "` + msg + `"}`))
	}

	checkAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return false
		}
		return true
	}

	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"id": 1, "name": "admin", "role": "admin"})
	})
	mux.HandleFunc("/api/cluster/info", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{
			"node_name": "node-a", "server_version": "29.8.0",
			"os": "linux", "arch": "amd64", "cpus": 8, "memory_bytes": 16106127360,
			"swarm": map[string]any{"state": "active", "control_available": true, "nodes": 2, "managers": 1},
		})
	})
	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"nodes": []map[string]any{
			{"id": "n1", "hostname": "node-a", "role": "manager", "is_leader": true},
			{"id": "n2", "hostname": "node-b", "role": "worker", "is_leader": false},
		}})
	})
	mux.HandleFunc("/api/stacks", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			write(w, map[string]any{"stacks": []map[string]any{
				{"name": "demo", "current_revision": 3, "repo_url": "https://github.com/acme/demo", "source_file": "deploy/demo.yaml"},
			}})
		case http.MethodPost:
			var p map[string]any
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				writeErr(w, http.StatusBadRequest, "bad body")
				return
			}
			write(w, map[string]any{"stack": "demo", "revision": 4, "changed": true})
		}
	})
	mux.HandleFunc("/api/stacks/demo", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			write(w, map[string]any{
				"stack":     map[string]any{"name": "demo", "current_revision": 3, "repo_url": "https://github.com/acme/demo", "source_file": "deploy/demo.yaml"},
				"revisions": []map[string]any{{"revision": 3, "created_at": 100}},
			})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/api/stacks/demo/revisions/3", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{
			"revision": 3, "stack": "demo",
			"source_yaml": "app: demo\n", "rendered_yaml": "services:\n  web:\n    image: nginx\n",
		})
	})
	mux.HandleFunc("/api/stacks/demo/sync", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"stack": "demo", "revision": 3, "changed": false})
	})
	mux.HandleFunc("/api/stacks/demo/rollback", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"stack": "demo", "new_revision": 5, "rolled_back_to": 3})
	})
	mux.HandleFunc("/api/backups", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		if r.Method == http.MethodPost {
			write(w, map[string]any{"id": 1, "status": "succeeded", "stack_name": "", "archive_paths": []string{"/var/stack/backup/b.tgz"}, "started_at": 100, "finished_at": 200})
			return
		}
		write(w, map[string]any{"backups": []map[string]any{{"id": 1, "status": "succeeded", "stack_name": "", "started_at": 100, "finished_at": 200}}})
	})
	mux.HandleFunc("/api/backups/1/files", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"run": map[string]any{"id": 1, "status": "succeeded", "started_at": 100, "finished_at": 200}, "files": []map[string]any{{"path": "demo/db.sqlite", "size": 42, "is_dir": false}}})
	})
	mux.HandleFunc("/api/backups/1/restore", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"restored": 3, "dest_root": "/var/stack/data"})
	})
	mux.HandleFunc("/api/tls/site", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"domain": "example.com", "expires": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), "cert_secret": "cert_v001", "key_secret": "key_v001"})
	})
	mux.HandleFunc("/api/tls/hosts", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"hosts": []map[string]any{{"host": "site.example.com", "cert_secret": "hc_v001"}}})
	})
	mux.HandleFunc("/api/webhooks", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		if r.Method == http.MethodPost {
			write(w, map[string]any{"source": "github-main", "secret": "s3cr3t"})
			return
		}
		write(w, map[string]any{"sources": []map[string]any{{"source": "github-main", "description": "main"}}})
	})
	mux.HandleFunc("/api/webhooks/github-main/deliveries", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"deliveries": []map[string]any{{"id": 1, "status": "accepted", "stack_name": "demo", "revision": 3, "created_at": 100}}})
	})
	mux.HandleFunc("/api/api_keys", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		if r.Method == http.MethodPost {
			write(w, map[string]any{"id": 1, "name": "ci", "token": "pmc_x_y"})
			return
		}
		write(w, map[string]any{"keys": []map[string]any{{"id": 1, "name": "ci", "created_at": 100}}})
	})
	mux.HandleFunc("/api/secrets", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		if r.Method == http.MethodPost {
			write(w, map[string]any{"id": 1, "scope": "service", "stack": "demo", "name": "db_pass", "hash": "abc", "created_at": 100})
			return
		}
		write(w, map[string]any{"secrets": []map[string]any{{"id": 1, "scope": "service", "stack": "demo", "name": "db_pass", "hash": "abc", "created_at": 100}}})
	})
	mux.HandleFunc("/api/secrets/db_pass", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			write(w, map[string]any{"id": 1, "scope": "service", "stack": "demo", "name": "db_pass", "hash": "abc", "created_at": 100})
		case http.MethodPut:
			write(w, map[string]any{"id": 1, "scope": "service", "stack": "demo", "name": "db_pass", "hash": "def", "created_at": 100})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/api/secrets/db_pass/value", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"name": "db_pass", "value": "plaintext"})
	})
	mux.HandleFunc("/api/secrets/db_pass/stack", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"name": "db_pass", "scope": "service", "stack": "other"})
	})
	mux.HandleFunc("/api/configs", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		if r.Method == http.MethodPost {
			write(w, map[string]any{"id": 1, "scope": "service", "stack": "demo", "name": "app_cfg", "kind": "env", "version": "v1", "hash": "abc", "created_at": 100})
			return
		}
		write(w, map[string]any{"configs": []map[string]any{{"id": 1, "scope": "cluster", "name": "traefik_dynamic", "kind": "template", "version": "v1", "hash": "abc", "created_at": 100}}})
	})
	mux.HandleFunc("/api/configs/app_cfg", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			write(w, map[string]any{"id": 1, "scope": "service", "stack": "demo", "name": "app_cfg", "kind": "env", "version": "v1", "hash": "abc", "created_at": 100, "content": "mode=prod"})
		case http.MethodPut:
			write(w, map[string]any{"id": 1, "scope": "service", "stack": "demo", "name": "app_cfg", "kind": "env", "version": "v2", "hash": "def", "created_at": 100})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/api/configs/app_cfg/versions", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"versions": []map[string]any{{"id": 1, "hash": "abc", "created_at": 100}, {"id": 2, "hash": "def", "created_at": 200}}})
	})
	mux.HandleFunc("/api/configs/app_cfg/rollback", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"id": 1, "scope": "service", "stack": "demo", "name": "app_cfg", "kind": "env", "version": "v1", "hash": "abc", "created_at": 100})
	})
	mux.HandleFunc("/api/configs/app_cfg/stack", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"name": "app_cfg", "scope": "service", "stack": "other"})
	})
	mux.HandleFunc("/api/cluster/rendered", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"configs": []map[string]any{{"name": "traefik-dynamic", "rendered": "http:\n", "rendered_hash": "h", "rendered_at": 100}}})
	})
	mux.HandleFunc("/api/cluster/settings", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		if r.Method == http.MethodPut {
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			write(w, map[string]any{"settings": in["settings"]})
			return
		}
		write(w, map[string]any{"settings": map[string]string{"domain": "example.com", "volume_root": "/var/stack/data"}})
	})
	mux.HandleFunc("/api/usage", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"configs": map[string][]string{"shared": {"demo"}}, "secrets": map[string][]string{"db_pass": {"demo"}}})
	})
	mux.HandleFunc("/api/services", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"services": []map[string]any{
			{"name": "demo_web", "stack": "demo", "replicas": 2, "desired": 2, "image": "nginx:latest", "mode": "replicated", "run_once": false},
			{"name": "demo_migrate", "stack": "demo", "replicas": 0, "desired": 1, "image": "app:latest", "mode": "replicated", "run_once": true},
		}})
	})
	mux.HandleFunc("/api/services/demo/web/tasks", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"service": "demo_web", "tasks": []map[string]any{{"task_id": "t1", "node": "node-a", "slot": 1, "state": "running", "started_at": 100}}})
	})
	mux.HandleFunc("/api/services/demo/web/logs", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"service": "demo_web", "logs": []map[string]any{{"stream": "stdout", "line": "listening on :8080"}}})
	})
	mux.HandleFunc("/api/services/demo/web/restart", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"service": "demo_web", "restarted": true})
	})
	mux.HandleFunc("/api/services/demo/web/exec", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		write(w, map[string]any{"service": "demo_web", "exit_code": 0, "stdout": "root\n", "stderr": ""})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		mux.ServeHTTP(w, r)
	})
}

// newFakeDaemon starts the fake server and returns a client pointed at it.
func newFakeDaemon(t *testing.T) (*Client, *fakeDaemon) {
	t.Helper()
	f := &fakeDaemon{t: t, token: "test-token"}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := New(srv.URL, f.token, 10*time.Second)
	return c, f
}

func TestPmapi_AuthAndCore(t *testing.T) {
	c, f := newFakeDaemon(t)
	ctx := context.Background()

	me, err := c.Me(ctx)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if me.Name != "admin" {
		t.Fatalf("Me.Name = %q", me.Name)
	}

	ci, err := c.ClusterInfo(ctx)
	if err != nil {
		t.Fatalf("ClusterInfo: %v", err)
	}
	if ci.NodeName != "node-a" || ci.Swarm.State != "active" {
		t.Fatalf("ClusterInfo = %+v", ci)
	}

	nodes, err := c.Nodes(ctx)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(nodes) != 2 || !nodes[0].IsLeader {
		t.Fatalf("Nodes = %+v", nodes)
	}

	stacks, err := c.ListStacks(ctx)
	if err != nil {
		t.Fatalf("ListStacks: %v", err)
	}
	if len(stacks) != 1 || stacks[0].Name != "demo" || stacks[0].SourceFile != "deploy/demo.yaml" {
		t.Fatalf("ListStacks = %+v", stacks)
	}

	// verify auth header was sent on a request
	if len(f.requests) == 0 {
		t.Fatal("no requests recorded")
	}
}

func TestPmapi_StackOps(t *testing.T) {
	c, _ := newFakeDaemon(t)
	ctx := context.Background()

	d, err := c.Stack(ctx, "demo")
	if err != nil {
		t.Fatalf("Stack: %v", err)
	}
	if d.Stack.Name != "demo" || len(d.Revisions) != 1 {
		t.Fatalf("Stack = %+v", d)
	}

	rev, err := c.Revision(ctx, "demo", 3)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if rev.Revision != 3 || !strings.Contains(rev.RenderedYAML, "nginx") {
		t.Fatalf("Revision = %+v", rev)
	}

	res, err := c.Deploy(ctx, DeployPayload{AppName: "demo", Manifest: "app: demo\n"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !res.Changed || res.Revision != 4 {
		t.Fatalf("Deploy = %+v", res)
	}

	sync, err := c.SyncStack(ctx, "demo")
	if err != nil {
		t.Fatalf("SyncStack: %v", err)
	}
	if sync.Changed {
		t.Fatalf("SyncStack should be no-op, got %+v", sync)
	}

	rb, err := c.Rollback(ctx, "demo", 3)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rb.NewRevision != 5 {
		t.Fatalf("Rollback = %+v", rb)
	}

	if err := c.DeleteStack(ctx, "demo"); err != nil {
		t.Fatalf("DeleteStack: %v", err)
	}
}

func TestPmapi_BackupsAndTLS(t *testing.T) {
	c, _ := newFakeDaemon(t)
	ctx := context.Background()

	backups, err := c.ListBackups(ctx, 10)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 1 || backups[0].Status != "succeeded" {
		t.Fatalf("ListBackups = %+v", backups)
	}

	created, err := c.CreateBackup(ctx)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if created.ID != 1 {
		t.Fatalf("CreateBackup = %+v", created)
	}

	run, files, err := c.BrowseBackup(ctx, 1)
	if err != nil {
		t.Fatalf("BrowseBackup: %v", err)
	}
	if run.ID != 1 || len(files) != 1 || files[0].Path != "demo/db.sqlite" {
		t.Fatalf("BrowseBackup run=%+v files=%+v", run, files)
	}

	n, err := c.RestoreBackup(ctx, 1, "/var/stack/data", "", false)
	if err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if n != 3 {
		t.Fatalf("RestoreBackup restored = %d", n)
	}

	sc, err := c.SiteCert(ctx)
	if err != nil {
		t.Fatalf("SiteCert: %v", err)
	}
	if sc.Domain != "example.com" || sc.CertSecret != "cert_v001" {
		t.Fatalf("SiteCert = %+v", sc)
	}

	hosts, err := c.ListHostCerts(ctx)
	if err != nil {
		t.Fatalf("ListHostCerts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].Host != "site.example.com" {
		t.Fatalf("ListHostCerts = %+v", hosts)
	}
}

func TestPmapi_ConfigsAndSecrets(t *testing.T) {
	c, _ := newFakeDaemon(t)
	ctx := context.Background()

	cfgs, err := c.ListConfigs(ctx, "", "")
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(cfgs) != 1 || cfgs[0].Name != "traefik_dynamic" {
		t.Fatalf("ListConfigs = %+v", cfgs)
	}

	created, err := c.CreateConfig(ctx, "service", "demo", "app_cfg", "env", "mode=prod")
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if created.Name != "app_cfg" || created.Version != "v1" {
		t.Fatalf("CreateConfig = %+v", created)
	}

	got, err := c.Config(ctx, "app_cfg")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if got.Content != "mode=prod" {
		t.Fatalf("Config = %+v", got)
	}

	upd, err := c.UpdateConfig(ctx, "app_cfg", "mode=staging")
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if upd.Version != "v2" {
		t.Fatalf("UpdateConfig = %+v", upd)
	}

	vers, err := c.ConfigVersions(ctx, "app_cfg")
	if err != nil {
		t.Fatalf("ConfigVersions: %v", err)
	}
	if len(vers) != 2 {
		t.Fatalf("ConfigVersions = %+v", vers)
	}

	rb, err := c.RollbackConfig(ctx, "app_cfg", 1)
	if err != nil {
		t.Fatalf("RollbackConfig: %v", err)
	}
	if rb.Version != "v1" {
		t.Fatalf("RollbackConfig = %+v", rb)
	}

	if err := c.RetagConfig(ctx, "app_cfg", "service", "other"); err != nil {
		t.Fatalf("RetagConfig: %v", err)
	}

	if err := c.DeleteConfig(ctx, "app_cfg"); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}

	secs, err := c.ListSecrets(ctx, "service", "demo")
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(secs) != 1 || secs[0].Name != "db_pass" {
		t.Fatalf("ListSecrets = %+v", secs)
	}

	sc, err := c.CreateSecret(ctx, "service", "demo", "db_pass", "v1")
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if sc.Hash != "abc" {
		t.Fatalf("CreateSecret = %+v", sc)
	}

	val, err := c.RevealSecret(ctx, "db_pass")
	if err != nil {
		t.Fatalf("RevealSecret: %v", err)
	}
	if val.Value != "plaintext" {
		t.Fatalf("RevealSecret = %+v", val)
	}

	if err := c.RetagSecret(ctx, "db_pass", "service", "other"); err != nil {
		t.Fatalf("RetagSecret: %v", err)
	}

	if err := c.DeleteSecret(ctx, "db_pass"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
}

func TestPmapi_SettingsUsageServices(t *testing.T) {
	c, _ := newFakeDaemon(t)
	ctx := context.Background()

	settings, err := c.ClusterSettings(ctx)
	if err != nil {
		t.Fatalf("ClusterSettings: %v", err)
	}
	if settings["domain"] != "example.com" {
		t.Fatalf("ClusterSettings = %+v", settings)
	}

	upd, err := c.UpdateClusterSettings(ctx, map[string]string{"domain": "other.com"})
	if err != nil {
		t.Fatalf("UpdateClusterSettings: %v", err)
	}
	if upd["domain"] != "other.com" {
		t.Fatalf("UpdateClusterSettings = %+v", upd)
	}

	usage, err := c.Usage(ctx)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.Secrets["db_pass"] == nil || usage.Secrets["db_pass"][0] != "demo" {
		t.Fatalf("Usage = %+v", usage)
	}

	rendered, err := c.ListRenderedConfigs(ctx)
	if err != nil {
		t.Fatalf("ListRenderedConfigs: %v", err)
	}
	if len(rendered) != 1 || rendered[0].Name != "traefik-dynamic" {
		t.Fatalf("ListRenderedConfigs = %+v", rendered)
	}

	svcs, err := c.ListServices(ctx, "")
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(svcs) != 2 {
		t.Fatalf("ListServices = %+v", svcs)
	}
	if !svcs[1].RunOnce {
		t.Fatalf("migration service should be run_once, got %+v", svcs[1])
	}

	tasks, err := c.ServiceTasks(ctx, "demo", "web")
	if err != nil {
		t.Fatalf("ServiceTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].State != "running" {
		t.Fatalf("ServiceTasks = %+v", tasks)
	}

	logs, err := c.ServiceLogs(ctx, "demo", "web", 200)
	if err != nil {
		t.Fatalf("ServiceLogs: %v", err)
	}
	if len(logs) != 1 || logs[0].Line != "listening on :8080" {
		t.Fatalf("ServiceLogs = %+v", logs)
	}

	if err := c.RestartService(ctx, "demo", "web"); err != nil {
		t.Fatalf("RestartService: %v", err)
	}

	exec, err := c.ExecService(ctx, "demo", "web", []string{"whoami"})
	if err != nil {
		t.Fatalf("ExecService: %v", err)
	}
	if exec.ExitCode != 0 || strings.TrimSpace(exec.Stdout) != "root" {
		t.Fatalf("ExecService = %+v", exec)
	}
}

func TestPmapi_ErrorsAndConfig(t *testing.T) {
	c, _ := newFakeDaemon(t)
	ctx := context.Background()

	// Wrong token → daemon 401 → typed Error
	bad := New(c.baseURL(), "wrong-token", 5*time.Second)
	if _, err := bad.Me(ctx); err == nil {
		t.Fatal("Me with bad token: expected error")
	} else if pe, ok := err.(*Error); !ok || pe.Status != http.StatusUnauthorized {
		t.Fatalf("Me bad token error = %v (want *Error with 401)", err)
	}

	// Configured() reflects base+token presence
	if !c.Configured() {
		t.Fatal("Configured() should be true")
	}
	empty := New("", "", 5*time.Second)
	if empty.Configured() {
		t.Fatal("empty client Configured() should be false")
	}

	// SetBase/SetToken swap without a restart
	c.SetBase(c.baseURL())
	c.SetToken("test-token")
	if !c.Configured() {
		t.Fatal("reconfigured client should be Configured")
	}
}
