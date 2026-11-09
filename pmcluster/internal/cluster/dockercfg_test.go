package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergeCredHelpersPreservesAnUnrelatedEntry is the rule that makes this
// safe to run on a live node: docker's config is shared state, and every key
// pmcluster does not own must come back exactly as it went in.
func TestMergeCredHelpersPreservesAnUnrelatedEntry(t *testing.T) {
	existing := []byte(`{
  "auths": {
    "registry.example.com": {"auth": "dXNlcjpwYXNz"}
  },
  "credHelpers": {
    "registry.example.com": "secretservice"
  },
  "proxies": {"default": {"httpProxy": "http://proxy:3128"}}
}`)
	out, changed, err := MergeCredHelpers(existing, CredHelperHost, CredHelperSuffix)
	if err != nil {
		t.Fatalf("MergeCredHelpers: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want the ghcr.io entry added")
	}

	var got struct {
		Auths       map[string]map[string]string `json:"auths"`
		CredHelpers map[string]string            `json:"credHelpers"`
		Proxies     map[string]map[string]string `json:"proxies"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("merged config is not JSON: %v\n%s", err, out)
	}
	if got.Auths["registry.example.com"]["auth"] != "dXNlcjpwYXNz" {
		t.Errorf("auths were not preserved: %+v", got.Auths)
	}
	if got.CredHelpers["registry.example.com"] != "secretservice" {
		t.Errorf("the other registry's helper was lost: %+v", got.CredHelpers)
	}
	if got.CredHelpers[CredHelperHost] != CredHelperSuffix {
		t.Errorf("credHelpers[%s] = %q, want %q", CredHelperHost, got.CredHelpers[CredHelperHost], CredHelperSuffix)
	}
	if got.Proxies["default"]["httpProxy"] != "http://proxy:3128" {
		t.Errorf("proxies were not preserved: %+v", got.Proxies)
	}
}

// TestMergeCredHelpersIsIdempotent: the second run must report no change and
// return the bytes untouched, so a reconcile does not rewrite (and re-read) the
// file on every pass.
func TestMergeCredHelpersIsIdempotent(t *testing.T) {
	first, changed, err := MergeCredHelpers(nil, CredHelperHost, CredHelperSuffix)
	if err != nil || !changed {
		t.Fatalf("first merge = (%v, %v), want a change", changed, err)
	}
	second, changed, err := MergeCredHelpers(first, CredHelperHost, CredHelperSuffix)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if changed {
		t.Error("second merge rewrote an already-correct entry")
	}
	if string(second) != string(first) {
		t.Errorf("second merge changed the bytes:\n%s\n%s", first, second)
	}
}

// TestMergeCredHelpersRepairsAWrongSuffix: ghcr.io pointed at a different
// helper is the App's host with the wrong credential — the App is the intended
// credential, so the entry is corrected.
func TestMergeCredHelpersRepairsAWrongSuffix(t *testing.T) {
	existing := []byte(`{"credHelpers":{"ghcr.io":"osxkeychain"}}`)
	out, changed, err := MergeCredHelpers(existing, CredHelperHost, CredHelperSuffix)
	if err != nil || !changed {
		t.Fatalf("merge = (%v, %v), want a change", changed, err)
	}
	var got struct {
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CredHelpers[CredHelperHost] != CredHelperSuffix {
		t.Errorf("credHelpers[%s] = %q, want %q", CredHelperHost, got.CredHelpers[CredHelperHost], CredHelperSuffix)
	}
}

// TestMergeCredHelpersRefusesNonJSON: a file that is not JSON may hold
// something an operator needs, so it is never overwritten with a fresh one.
func TestMergeCredHelpersRefusesNonJSON(t *testing.T) {
	if _, changed, err := MergeCredHelpers([]byte("not json at all"), CredHelperHost, CredHelperSuffix); err == nil || changed {
		t.Fatalf("merge of a non-JSON file = (%v, %v), want an error and no change", changed, err)
	}
}

// TestEnsureCredentialHelperIsIdempotent covers both host mutations against a
// temporary tree: the symlink resolves to the binary, the config carries the
// entry, and a second run changes nothing.
func TestEnsureCredentialHelperIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "bin", "pmcluster")
	link := filepath.Join(dir, "usr-local-bin", "docker-credential-"+CredHelperSuffix)
	config := filepath.Join(dir, "root", ".docker", "config.json")

	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	// An existing unrelated registry entry, to prove the merge does not clobber.
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.WriteFile(config, []byte(`{"auths":{"registry.example.com":{"auth":"abc"}}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := ensureCredentialHelperAt(binary, link, config); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}

	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("symlink was not installed: %v", err)
	}
	assertSamePath(t, target, binary)
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got struct {
		Auths       map[string]map[string]string `json:"auths"`
		CredHelpers map[string]string            `json:"credHelpers"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("config is not JSON: %v\n%s", err, raw)
	}
	if got.Auths["registry.example.com"]["auth"] != "abc" {
		t.Errorf("the unrelated auth entry was clobbered: %s", raw)
	}
	if got.CredHelpers[CredHelperHost] != CredHelperSuffix {
		t.Errorf("credHelpers[%s] = %q, want %q: %s", CredHelperHost, got.CredHelpers[CredHelperHost], CredHelperSuffix, raw)
	}
	st, err := os.Stat(config)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if st.Mode().Perm() != dockerConfigMode {
		t.Errorf("config mode = %o, want %o", st.Mode().Perm(), dockerConfigMode)
	}
}

// TestEnsureCredentialHelperReplacesAStaleSymlink: an install that moved has to
// leave a working helper, not a symlink to a path that no longer exists.
func TestEnsureCredentialHelperReplacesAStaleSymlink(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "pmcluster")
	link := filepath.Join(dir, "docker-credential-"+CredHelperSuffix)
	config := filepath.Join(dir, "config.json")

	if err := os.WriteFile(binary, []byte("x"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.Symlink("/opt/old/pmcluster", link); err != nil {
		t.Fatalf("seed stale symlink: %v", err)
	}
	if err := ensureCredentialHelperAt(binary, link, config); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	assertSamePath(t, target, binary)
}

// TestEnsureCredentialHelperRefusesARegularFile: something an operator put at
// that path is not ours to delete.
func TestEnsureCredentialHelperRefusesARegularFile(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "pmcluster")
	link := filepath.Join(dir, "docker-credential-"+CredHelperSuffix)
	config := filepath.Join(dir, "config.json")

	if err := os.WriteFile(binary, []byte("x"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.WriteFile(link, []byte("not a symlink"), 0o755); err != nil {
		t.Fatalf("write file: %v", err)
	}
	err := ensureCredentialHelperAt(binary, link, config)
	if err == nil {
		t.Fatal("a regular file at the helper path was replaced")
	}
	if !strings.Contains(err.Error(), link) {
		t.Errorf("error = %v, want it to name %s", err, link)
	}
	if _, statErr := os.Stat(config); !os.IsNotExist(statErr) {
		t.Error("the config was written even though the symlink failed")
	}
}

// TestEnsureDockerCredentialHelperUsesTheRunningBinary: the production entry
// point links to os.Executable(), resolved, because /usr/local/bin/pmcluster is
// usually itself a symlink and docker needs a path that survives an upgrade.
func TestEnsureDockerCredentialHelperUsesTheRunningBinary(t *testing.T) {
	dir := t.TempDir()
	realBin := filepath.Join(dir, "pmcluster-1.2.3")
	if err := os.WriteFile(realBin, []byte("x"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	plugin := filepath.Join(dir, "pmcluster")
	if err := os.Symlink(realBin, plugin); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	link := filepath.Join(dir, "docker-credential-"+CredHelperSuffix)
	config := filepath.Join(dir, "config.json")

	restoreExe, restoreLink, restoreConfig := osExecutable, credHelperLinkPath, dockerConfigFilePath
	defer func() {
		osExecutable, credHelperLinkPath, dockerConfigFilePath = restoreExe, restoreLink, restoreConfig
	}()
	osExecutable = func() (string, error) { return plugin, nil }
	credHelperLinkPath = link
	dockerConfigFilePath = config

	if err := ensureDockerCredentialHelperReal(); err != nil {
		t.Fatalf("ensureDockerCredentialHelperReal: %v", err)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	assertSamePath(t, target, realBin)
	if _, err := os.Stat(config); err != nil {
		t.Errorf("config was not written: %v", err)
	}
}

// assertSamePath compares two paths after resolving symlinks — macOS's
// /var → /private/var makes the raw strings differ for the same file.
func assertSamePath(t *testing.T, got, want string) {
	t.Helper()
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	if resolve(got) != resolve(want) {
		t.Errorf("symlink target = %q, want %q", got, want)
	}
}
