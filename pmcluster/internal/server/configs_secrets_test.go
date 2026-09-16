package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestSecretsAPI exercises GET/POST/DELETE /api/secrets: list without payload,
// create with encrypted storage, delete by name.
func TestSecretsAPI(t *testing.T) {
	srv := newKeysServer(t)
	base := srv.URL + "/api/secrets"

	if resp := doJSON(t, http.MethodGet, base, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth GET /secrets = %d, want 401", resp.StatusCode)
	}

	resp := doJSON(t, http.MethodGet, base, "tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /secrets = %d, want 200", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodPost, base, "tok", map[string]string{"scope": "service", "name": "db_pass", "value": "hunter2"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /secrets = %d, want 201; body: %s", resp.StatusCode, readBody(t, resp))
	}
	created := decodeBody[struct {
		ID    int64  `json:"id"`
		Scope string `json:"scope"`
		Name  string `json:"name"`
		Hash  string `json:"hash"`
	}](t, resp)
	if created.Name != "db_pass" || created.Scope != "service" {
		t.Errorf("created = %+v, want db_pass/service", created)
	}
	if len(created.Hash) != 64 {
		t.Errorf("hash = %q, want 64-hex sha256", created.Hash)
	}

	resp = doJSON(t, http.MethodPost, base, "tok", map[string]string{"name": "db_pass", "value": "x"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("dup POST /secrets = %d, want 409", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodPost, base, "tok", map[string]string{"scope": "global", "name": "nope", "value": "x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-scope POST /secrets = %d, want 400", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodGet, base, "tok", nil)
	body := readBody(t, resp)
	if !strings.Contains(body, "db_pass") || !strings.Contains(body, created.Hash) {
		t.Errorf("list missing secret; got: %s", body)
	}
	if strings.Contains(body, "hunter2") {
		t.Errorf("list leaked secret payload: %s", body)
	}

	resp = doJSON(t, http.MethodDelete, base+"/db_pass", "tok", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /secrets/db_pass = %d, want 204", resp.StatusCode)
	}
	resp = doJSON(t, http.MethodDelete, base+"/db_pass", "tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("re-DELETE /secrets/db_pass = %d, want 404", resp.StatusCode)
	}
}

// TestConfigsAPI exercises the full config lifecycle: create, list, get,
// update (with version history), versions, rollback, delete.
func TestConfigsAPI(t *testing.T) {
	srv := newKeysServer(t)
	base := srv.URL + "/api/configs"

	if resp := doJSON(t, http.MethodGet, base, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth GET /configs = %d, want 401", resp.StatusCode)
	}

	resp := doJSON(t, http.MethodPost, base, "tok", map[string]string{"scope": "service", "name": "nginx_conf", "kind": "file", "content": "worker_processes 4;"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /configs = %d, want 201; body: %s", resp.StatusCode, readBody(t, resp))
	}
	created := decodeBody[struct {
		ID    int64  `json:"id"`
		Scope string `json:"scope"`
		Name  string `json:"name"`
		Kind  string `json:"kind"`
		Hash  string `json:"hash"`
	}](t, resp)
	if created.Name != "nginx_conf" || created.Scope != "service" || created.Kind != "file" {
		t.Errorf("created = %+v, want nginx_conf/service/file", created)
	}
	if len(created.Hash) != 64 {
		t.Errorf("hash = %q, want 64-hex sha256", created.Hash)
	}

	resp = doJSON(t, http.MethodPost, base, "tok", map[string]string{"name": "nginx_conf", "content": "x"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("dup POST /configs = %d, want 409", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodPost, base, "tok", map[string]string{"name": "weird", "kind": "binary", "content": "x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-kind POST /configs = %d, want 400", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodGet, base, "tok", nil)
	body := readBody(t, resp)
	if !strings.Contains(body, "nginx_conf") {
		t.Errorf("list missing config; got: %s", body)
	}

	resp = doJSON(t, http.MethodGet, base+"/nginx_conf", "tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /configs/nginx_conf = %d, want 200", resp.StatusCode)
	}
	got := readBody(t, resp)
	if !strings.Contains(got, "worker_processes 4;") {
		t.Errorf("get missing content; got: %s", got)
	}

	resp = doJSON(t, http.MethodGet, base+"/missing", "tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /configs/missing = %d, want 404", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodPut, base+"/nginx_conf", "tok", map[string]string{"content": "worker_processes 8;"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /configs/nginx_conf = %d, want 200; body: %s", resp.StatusCode, readBody(t, resp))
	}
	updated := decodeBody[struct {
		Name string `json:"name"`
		Hash string `json:"hash"`
	}](t, resp)
	if updated.Hash == created.Hash {
		t.Errorf("update did not change hash: %s", updated.Hash)
	}

	resp = doJSON(t, http.MethodGet, base+"/nginx_conf/versions", "tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /configs/nginx_conf/versions = %d, want 200", resp.StatusCode)
	}
	var versions struct {
		Versions []struct {
			ID   int64  `json:"id"`
			Hash string `json:"hash"`
		} `json:"versions"`
	}
	vb := decodeBody[struct {
		Versions []struct {
			ID   int64  `json:"id"`
			Hash string `json:"hash"`
		} `json:"versions"`
	}](t, resp)
	versions = vb
	if len(versions.Versions) != 1 {
		t.Fatalf("versions = %+v, want exactly 1 (the pre-update content)", versions.Versions)
	}
	if versions.Versions[0].Hash != created.Hash {
		t.Errorf("history hash = %s, want %s (pre-update content)", versions.Versions[0].Hash, created.Hash)
	}

	resp = doJSON(t, http.MethodPost, base+"/nginx_conf/rollback", "tok", map[string]any{"version_id": versions.Versions[0].ID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /configs/nginx_conf/rollback = %d, want 200; body: %s", resp.StatusCode, readBody(t, resp))
	}
	rb := decodeBody[struct {
		Name         string `json:"name"`
		Hash         string `json:"hash"`
		RolledBackTo int64  `json:"rolled_back_to"`
	}](t, resp)
	if rb.RolledBackTo != versions.Versions[0].ID || rb.Hash != created.Hash {
		t.Errorf("rollback = %+v, want hash %s at version %d", rb, created.Hash, versions.Versions[0].ID)
	}

	resp = doJSON(t, http.MethodPost, base+"/nginx_conf/rollback", "tok", map[string]any{"version_id": 999})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bogus rollback = %d, want 404", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodDelete, base+"/nginx_conf", "tok", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /configs/nginx_conf = %d, want 204", resp.StatusCode)
	}
	resp = doJSON(t, http.MethodDelete, base+"/nginx_conf", "tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("re-DELETE /configs/nginx_conf = %d, want 404", resp.StatusCode)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}
