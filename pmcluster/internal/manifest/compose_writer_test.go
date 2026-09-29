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
