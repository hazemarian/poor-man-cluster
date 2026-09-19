package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
)

// TestClusterSettingsAPI exercises GET/PUT /api/cluster/settings: auth, the
// known-key allowlist on read, a valid update round-trip into the store, and
// rejection of unknown keys with 400.
func TestClusterSettingsAPI(t *testing.T) {
	st := openServerStore(t)
	ctx := context.Background()

	srv := httptest.NewServer(New(Deps{
		Lookup: &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Store:  st,
	}))
	defer srv.Close()
	base := srv.URL + "/api/cluster/settings"

	t.Run("unauth → 401", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("GET returns known keys", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, readBody(t, resp))
		}
		var body struct {
			Settings map[string]string `json:"settings"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, key := range []string{
			cluster.SettingVolumeRoot(),
			cluster.SettingBackupAllNodes(),
			cluster.SettingDomain(),
			cluster.SettingSSOEnabled(),
			cluster.SettingSSOClientSecret(),
			cluster.SettingTraefikAdminUser(),
		} {
			if _, ok := body.Settings[key]; !ok {
				t.Errorf("settings missing known key %q; got %v", key, body.Settings)
			}
		}
	})

	t.Run("PUT valid key updates store", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base, "tok", map[string]any{
			"settings": map[string]string{"volume_root": "/var/stack/data"},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, readBody(t, resp))
		}
		var body struct {
			Settings map[string]string `json:"settings"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Settings["volume_root"] != "/var/stack/data" {
			t.Errorf("response volume_root = %q, want /var/stack/data", body.Settings["volume_root"])
		}
		if got := st.GetSettingDefault(ctx, "volume_root", ""); got != "/var/stack/data" {
			t.Errorf("stored volume_root = %q, want /var/stack/data", got)
		}
	})

	t.Run("PUT unknown key → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base, "tok", map[string]any{
			"settings": map[string]string{"bogus_key": "x"},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("PUT mixed known+unknown → 400, nothing written", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base, "tok", map[string]any{
			"settings": map[string]string{"domain": "evil.example", "bogus_key": "x"},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		if got := st.GetSettingDefault(ctx, "domain", ""); got == "evil.example" {
			t.Errorf("unknown-key rejection still wrote the valid key (domain = %q)", got)
		}
	})
}
