package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

// TestTranslate_DependsOnEmitsComposeParity builds a migration service that
// depends on the db service and asserts the rendered compose carries the
// depends_on block (condition service_started) for compose parity.
func TestTranslate_DependsOnEmitsComposeParity(t *testing.T) {
	app := baseApp()
	app.Services = map[string]*dsl.Service{
		"db": {
			Image:       "postgres:14-alpine",
			Volumes:     []string{"db_data:/var/lib/postgresql/data"},
			Healthcheck: &dsl.Healthcheck{Type: "pg_isready"},
		},
		"migration": {
			Image:     "my-app:latest",
			Command:   []string{"./migrate"},
			RunOnce:   true,
			DependsOn: []string{"db"},
		},
	}

	out, err := TranslateIR(context.Background(), app, nil, nil)
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}
	s := string(out)

	// Compose-parity depends_on block present for the migration service
	// (list form — compose v3.9 / docker stack deploy rejects the map form).
	if !strings.Contains(s, "depends_on:") {
		t.Errorf("rendered compose should contain a depends_on block:\n%s", s)
	}
	if !strings.Contains(s, "depends_on:\n    - db") {
		t.Errorf("depends_on should reference db as a list item:\n%s", s)
	}

	// The migration command is wrapped with the swarm wait loop (docker stack
	// deploy ignores depends_on; the wrapper blocks until db DNS resolves).
	// Literal $ in the wrapper is doubled ($$) so docker stack deploy's own
	// ${...} interpolation passes it through untouched.
	if !strings.Contains(s, `for d in db; do until getent hosts "$$d"`) {
		t.Errorf("migration command should be wrapped with the db wait loop:\n%s", s)
	}
	// postgres:14-alpine has a well-known port (5432), so the wait must also
	// probe the connection — a DNS answer alone does not mean db is up.
	if !strings.Contains(s, `nc -z "$$d" 5432`) {
		t.Errorf("wait wrapper should probe the well-known postgres port:\n%s", s)
	}
	if !strings.Contains(s, `exec "$$@"`) {
		t.Errorf("wait wrapper should exec the original command:\n%s", s)
	}

	// A run-once job that waits on dependencies gets a bounded on-failure
	// restart policy (so a transient failure retries, then stops).
	if !strings.Contains(s, "condition: on-failure") {
		t.Errorf("run-once + depends_on should use on-failure restart:\n%s", s)
	}
	if !strings.Contains(s, "max_attempts: 3") {
		t.Errorf("run-once + depends_on should bound retries to 3:\n%s", s)
	}
}

// TestTranslate_DependsOnWrapsBakedEntrypoint asserts a service with
// depends_on but NO explicit command/entrypoint gets its ENTRYPOINT wrapped
// with the wait loop — compose forwards the image's baked CMD as "$@", so the
// image's own runnable still runs after the dependency wait.
func TestTranslate_DependsOnWrapsBakedEntrypoint(t *testing.T) {
	app := baseApp()
	app.Services = map[string]*dsl.Service{
		"db": {
			Image:   "postgres:14-alpine",
			Volumes: []string{"db_data:/var/lib/postgresql/data"},
		},
		"migration": {
			Image:     "my-app:latest", // image ships the migration in its baked CMD
			RunOnce:   true,
			DependsOn: []string{"db"},
		},
	}

	out, err := TranslateIR(context.Background(), app, nil, nil)
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}
	s := string(out)

	// The entrypoint carries the wait loop; the image's baked CMD flows
	// through as "$@" untouched. Literal $ is doubled ($$) so docker stack
	// deploy's ${...} interpolation leaves it alone.
	if !strings.Contains(s, `for d in db; do until getent hosts "$$d"`) {
		t.Errorf("migration entrypoint should carry the db wait loop:\n%s", s)
	}
	// postgres:14-alpine has a well-known port (5432) — the wait probes it.
	if !strings.Contains(s, `nc -z "$$d" 5432`) {
		t.Errorf("wait wrapper should probe the well-known postgres port:\n%s", s)
	}
	if !strings.Contains(s, `exec "$$@"`) {
		t.Errorf("wait wrapper should exec the image's own command:\n%s", s)
	}
	if !strings.Contains(s, "entrypoint:") {
		t.Errorf("no-command service should wrap via entrypoint:\n%s", s)
	}
	if !strings.Contains(s, "depends_on:\n    - db") {
		t.Errorf("depends_on list form should still be emitted:\n%s", s)
	}
}

// TestServicePortInfersDefault resolves the port a dependency listens on:
// the expose port wins, then a well-known image default, else 0 (DNS-only).
func TestServicePortInfersDefault(t *testing.T) {
	cases := []struct {
		name string
		svc  dsl.Service
		want int
	}{
		{"expose port wins", dsl.Service{Image: "ghcr.io/acme/app:1", Expose: &dsl.Expose{Port: 8080}}, 8080},
		{"postgres well-known", dsl.Service{Image: "postgres:14-alpine"}, 5432},
		{"redis well-known", dsl.Service{Image: "redis:7-alpine"}, 6379},
		{"registry path stripped", dsl.Service{Image: "ghcr.io/org/postgres:16"}, 5432},
		{"unknown image", dsl.Service{Image: "ghcr.io/acme/custom-app:1"}, 0},
		{"empty image", dsl.Service{Image: ""}, 0},
	}
	for _, tc := range cases {
		if got := servicePort(&tc.svc); got != tc.want {
			t.Errorf("%s: servicePort() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestEnsureVolumeDirs_CreatesBindTargets renders a compose with a named
// volume and asserts EnsureVolumeDirs mkdirs the driver_opts device paths.
func TestEnsureVolumeDirs_CreatesBindTargets(t *testing.T) {
	app := baseApp()
	app.Name = "vault-app"
	app.Services = map[string]*dsl.Service{
		"api": {
			Image:   "nginx:latest",
			Volumes: []string{"data_vol:/data"},
		},
	}

	out, err := TranslateIR(context.Background(), app, nil, &ComposeWriter{})
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}

	root := DefaultVolumeRoot
	var made []string
	mkdirAll := func(dir string, mode os.FileMode) error {
		made = append(made, dir)
		return nil
	}

	if err := EnsureVolumeDirs(out, mkdirAll); err != nil {
		t.Fatalf("EnsureVolumeDirs: %v", err)
	}

	want := filepath.Join(root, "vault-app", "data_vol")
	found := false
	for _, d := range made {
		if d == want {
			found = true
		}
	}
	if !found {
		t.Errorf("EnsureVolumeDirs should have created %q, got %v", want, made)
	}
}

// TestEnsureVolumeDirs_SkipsNonBindVolumes asserts volumes without a
// driver_opts device (e.g. external or plain named volumes) are skipped.
func TestEnsureVolumeDirs_SkipsNonBindVolumes(t *testing.T) {
	// No driver_opts device — must not be created, must not error.
	err := EnsureVolumeDirs([]byte("volumes:\n  plain:\n    driver: local\n"), func(string, os.FileMode) error {
		t.Fatal("mkdirAll should not be called for a volume without device")
		return nil
	})
	if err != nil {
		t.Fatalf("EnsureVolumeDirs: %v", err)
	}
}

// TestTranslate_CertResolverConditional asserts the tls.certresolver label is
// emitted on exposed routers ONLY when the writer carries a resolver name
// (ACME clusters). BYO-cert clusters leave it empty and must get NO label —
// referencing a nonexistent letsencrypt resolver would break routing.
func TestTranslate_CertResolverConditional(t *testing.T) {
	app := baseApp()
	app.Services = map[string]*dsl.Service{
		"api": {
			Image: "nginx:latest",
			Expose: &dsl.Expose{
				Port: 8080,
				Host: "api.example.com",
			},
		},
	}

	// ACME cluster: resolver present.
	out, err := TranslateIR(context.Background(), app, nil, &ComposeWriter{CertResolver: "letsencrypt"})
	if err != nil {
		t.Fatalf("TranslateIR (acme): %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "tls.certresolver: letsencrypt") {
		t.Errorf("ACME render should carry tls.certresolver: letsencrypt:\n%s", s)
	}

	// BYO-cert cluster: no resolver label at all.
	out, err = TranslateIR(context.Background(), app, nil, &ComposeWriter{})
	if err != nil {
		t.Fatalf("TranslateIR (byo): %v", err)
	}
	s = string(out)
	if strings.Contains(s, "tls.certresolver") {
		t.Errorf("BYO-cert render must NOT reference a letsencrypt resolver:\n%s", s)
	}
}

// TestTranslate_StatefulDefaults asserts that a service holding a volume gets
// stateful-aware defaults with NO manifest change: automatic stop-first update
// ordering (kills the postgres postmaster.pid shutdown race on redeploys) and
// auto-pinning to the platform node when no explicit placement is set.
func TestTranslate_StatefulDefaults(t *testing.T) {
	app := baseApp()
	app.Services = map[string]*dsl.Service{
		"db": {
			Image:   "postgres:14-alpine",
			Volumes: []string{"db_data:/var/lib/postgresql/data"},
		},
	}

	out, err := TranslateIR(context.Background(), app, nil, &ComposeWriter{PinNode: "node-01"})
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}
	s := string(out)

	// Stop-first: the old task shuts down fully before the replacement starts.
	if !strings.Contains(s, "order: stop-first") {
		t.Errorf("stateful service should default to stop-first update order:\n%s", s)
	}

	// Auto-pin: no explicit placement → pinned to the platform node.
	if !strings.Contains(s, "node.hostname == node-01") {
		t.Errorf("stateful service without placement should auto-pin to the platform node:\n%s", s)
	}
}

// TestTranslate_StatefulDefaultsDisabled asserts that without a PinNode the
// auto-pin is off (stateful services schedule anywhere) and stateless services
// keep start-first ordering even when a PinNode is configured.
func TestTranslate_StatefulDefaultsDisabled(t *testing.T) {
	app := baseApp()
	app.Services = map[string]*dsl.Service{
		"db": {
			Image:   "postgres:14-alpine",
			Volumes: []string{"db_data:/var/lib/postgresql/data"},
		},
		"web": {
			Image: "nginx:latest",
		},
	}

	out, err := TranslateIR(context.Background(), app, nil, nil)
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}
	s := string(out)

	if strings.Contains(s, "node.hostname") {
		t.Errorf("no PinNode configured — stateful services must not be pinned:\n%s", s)
	}
	// Stateless web service keeps the start-first default.
	if !strings.Contains(s, "order: start-first") {
		t.Errorf("stateless service should keep start-first update order:\n%s", s)
	}
}
