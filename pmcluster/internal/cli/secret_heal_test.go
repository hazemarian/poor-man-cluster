package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/secrets"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// fakeSecretDocker records swarm secret operations for the heal run path.
type fakeSecretDocker struct {
	runtime.Client
	existing map[string]bool
	created  []string
}

func (f *fakeSecretDocker) Close() error { return nil }

func (f *fakeSecretDocker) SecretExists(_ context.Context, name string) (bool, error) {
	return f.existing[name], nil
}

func (f *fakeSecretDocker) SecretCreate(_ context.Context, spec runtime.SecretSpec) error {
	f.created = append(f.created, spec.Name)
	f.existing[spec.Name] = true
	return nil
}

func TestSecretHealCommandRegistered(t *testing.T) {
	var found bool
	for _, c := range secretCmd.Commands() {
		if c.Name() == "heal" {
			found = true
			if c.Flags().Lookup("dry-run") == nil {
				t.Error("secret heal is missing the --dry-run flag")
			}
		}
	}
	if !found {
		t.Fatal("secret heal is not registered under the secret command")
	}
}

func TestSecretHealRunPath(t *testing.T) {
	cfg, cleanupEnv := newTestCLIEnv(t)
	defer cleanupEnv()

	// Seed one secret encrypted with this environment's key.
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		t.Fatalf("open cipher: %v", err)
	}
	if _, err := secrets.NewLocal(st, cipher).Create(context.Background(), "service", "demo", "heal_pass", "value-heal"); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	fd := &fakeSecretDocker{existing: map[string]bool{}}
	origDocker := dockerNewFn
	dockerNewFn = func() (runtime.Client, error) { return fd, nil }
	defer func() { dockerNewFn = origDocker }()
	origAPI := apiURL
	apiURL = ""
	defer func() { apiURL = origAPI }()

	cmd, out, _ := newTestCmd("heal", nil, runSecretHeal)
	if err := runSecretHeal(cmd, nil); err != nil {
		t.Fatalf("runSecretHeal: %v", err)
	}

	want := store.SwarmSecretName("heal_pass", store.SecretHash("value-heal"))
	if len(fd.created) != 1 || fd.created[0] != want {
		t.Errorf("created swarm secrets = %v, want [%s]", fd.created, want)
	}
	body := out.String()
	for _, want := range []string{"repaired", "swarm mirror created", "1 secret(s) checked: 0 ok, 1 repaired"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
}
