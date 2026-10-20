package cluster

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"sigs.k8s.io/yaml"
)

func TestNativeEdgeSecretMountIsOptionalAndVersioned(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		in := RenderInput{Domain: "example.test", EdgeImage: "edge:fixture", ObservabilityAdminEmail: "ops@example.test"}
		if enabled {
			in.ManagedSecretNames = map[string]string{"investigation_provider_key": "investigation_provider_key_12345678"}
		}
		body, err := LoadComposeFile(StackEdge, in)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Services map[string]struct {
				Secrets  []string `json:"secrets"`
				Networks []string `json:"networks"`
			} `json:"services"`
			Secrets map[string]struct {
				Name string `json:"name"`
			} `json:"secrets"`
		}
		if err = yaml.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		secret, exists := doc.Secrets["investigation_provider_key"]
		if exists != enabled {
			t.Fatalf("enabled=%v secret mount=%v", enabled, exists)
		}
		if enabled && secret.Name != "investigation_provider_key_12345678" {
			t.Fatalf("unversioned source: %+v", secret)
		}
		foundNet, foundMount := false, false
		for _, svc := range doc.Services {
			for _, name := range svc.Networks {
				if name == "monitoring-net" {
					foundNet = true
				}
			}
			for _, name := range svc.Secrets {
				if name == "investigation_provider_key" {
					foundMount = true
				}
			}
		}
		if !foundNet || foundMount != enabled {
			t.Fatalf("network=%v mount=%v", foundNet, foundMount)
		}
	}
}

func TestNativeProviderKeyUsesImportedGenericSecret(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	first := store.SecretHash("fixture-one")
	if _, err = st.CreateSecret(ctx, "cluster", "", "investigation_provider_key", []byte("encrypted-fixture"), first); err != nil {
		t.Fatal(err)
	}
	if got := managedSecretNames(ctx, st)["investigation_provider_key"]; got != store.SwarmSecretName("investigation_provider_key", first) {
		t.Fatalf("source=%s", got)
	}
	second := store.SecretHash("fixture-two")
	if err = st.UpdateSecret(ctx, "investigation_provider_key", []byte("encrypted-fixture"), second); err != nil {
		t.Fatal(err)
	}
	if got := managedSecretNames(ctx, st)["investigation_provider_key"]; got != store.SwarmSecretName("investigation_provider_key", second) {
		t.Fatalf("rotation source=%s", got)
	}
}
