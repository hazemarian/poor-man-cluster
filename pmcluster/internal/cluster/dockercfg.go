package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The pull-time credential helper, installed where Docker looks for it.
//
// The helper is this same binary under a symlink: docker runs
// `docker-credential-<suffix> <verb>` for any registry a config.json
// `credHelpers` entry names, so `credHelpers: {"ghcr.io": "pmcluster"}` makes
// every ghcr.io pull ask `docker-credential-pmcluster` — which fetches a fresh
// installation token from the edge (see internal/pullcred) instead of reading a
// token that was written into docker's config an hour ago.
//
// Two host mutations, both idempotent, both reported as apply warnings when they
// fail (a node that cannot be provisioned must not abort the repair of
// everything else):
//
//  1. /usr/local/bin/docker-credential-pmcluster → the running pmcluster binary.
//  2. /root/.docker/config.json: MERGE credHelpers["ghcr.io"]="pmcluster",
//     preserving every other key — auths for other registries, credsStore,
//     proxies, whatever the operator or another tool put there.
//
// "Merge, never overwrite" is the whole rule for (2). This file is shared:
// an operator's `docker login` writes it, other helpers write it, and a
// pmcluster write that replaced it would silently log the node out of every
// registry it had. The merge is also a no-op when the entry is already right,
// so repeated `cluster update` runs leave the file byte-identical.

const (
	// DockerConfigPath is the daemon's docker config on a node. dockerd and the
	// docker CLI both run as root under systemd, so this is the file that
	// matters for a pull.
	DockerConfigPath = "/root/.docker/config.json"
	// CredHelperBin is where the symlink is installed. /usr/local/bin is on
	// dockerd's PATH (systemd's default includes it), which is what makes the
	// helper resolvable by the suffix docker constructs.
	CredHelperBin = "/usr/local/bin/docker-credential-pmcluster"
	// CredHelperSuffix is the name docker appends to "docker-credential-".
	// It must equal the symlink's basename — the helper dispatches on argv[0].
	CredHelperSuffix = "pmcluster"
	// CredHelperHost is the registry the helper answers for. It is the same
	// host the App's installation token authenticates against.
	CredHelperHost = "ghcr.io"
)

// dockerConfigMode is the mode docker itself writes: the file can hold
// credentials for other registries, so it is never group/world readable.
const dockerConfigMode os.FileMode = 0o600

// The host-mutation seams. They are package variables, like hostMkdirAll, so a
// test drives the real helper against a t.TempDir instead of /root.
var (
	ensureDockerCredentialHelper = ensureDockerCredentialHelperReal
	readDockerConfigFile         = os.ReadFile
	osExecutable                 = os.Executable
	// credHelperLinkPath and dockerConfigFilePath are the installed locations;
	// vars so a test can redirect them at a temporary tree.
	credHelperLinkPath   = CredHelperBin
	dockerConfigFilePath = DockerConfigPath
)

// EnsureDockerCredentialHelper installs the helper symlink and merges the
// ghcr.io credHelpers entry on THIS node.
//
// "On this node" is not a limitation of the code, it is what a host mutation
// is: `cluster up` and `cluster update` run on one node and may write only that
// node's /root. Every node therefore runs this for itself — the workflows call
// it, and the daemon calls it at startup (see the serve command) — which is the
// same shape as the platform host directories.
func EnsureDockerCredentialHelper() error { return ensureDockerCredentialHelper() }

func ensureDockerCredentialHelperReal() error {
	exe, err := osExecutable()
	if err != nil {
		return fmt.Errorf("locate the running pmcluster binary (its path becomes the credential helper): %w", err)
	}
	return ensureCredentialHelperAt(exe, credHelperLinkPath, dockerConfigFilePath)
}

// ensureCredentialHelperAt is the whole operation with its paths named, so a
// test can point it at a temporary tree.
func ensureCredentialHelperAt(binary, link, configPath string) error {
	if err := installCredHelperLink(binary, link); err != nil {
		return err
	}
	return mergeDockerConfigFile(configPath, CredHelperHost, CredHelperSuffix)
}

// installCredHelperLink points link at binary. Idempotent: an existing symlink
// with the right target is left alone, a symlink pointing somewhere else (a
// stale install path) is replaced, and a REGULAR file at that path is an error
// naming it — silently deleting something an operator put there is not this
// function's call.
func installCredHelperLink(binary, link string) error {
	target, err := filepath.EvalSymlinks(binary)
	if err != nil {
		// A binary reached through a symlink chain still has a real path; if it
		// cannot be resolved, link to the path we were given rather than fail.
		target = binary
	}
	st, err := os.Lstat(link)
	switch {
	case err == nil && st.Mode()&os.ModeSymlink != 0:
		if current, rerr := os.Readlink(link); rerr == nil && current == target {
			return nil
		}
		if rerr := os.Remove(link); rerr != nil {
			return fmt.Errorf("replace the stale credential-helper symlink %s: %w", link, rerr)
		}
	case err == nil:
		return fmt.Errorf("%s exists and is not a symlink — refusing to replace it; move it aside and re-run", link)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("inspect %s: %w", link, err)
	}

	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(link), err)
	}
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("install the credential helper at %s (it must be on dockerd's PATH): %w", link, err)
	}
	return nil
}

// mergeDockerConfigFile writes the merged config back, atomically and only when
// the merge changed something.
func mergeDockerConfigFile(configPath, host, suffix string) error {
	existing, err := readDockerConfigFile(configPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	merged, changed, err := MergeCredHelpers(existing, host, suffix)
	if err != nil {
		return fmt.Errorf("%s: %w", configPath, err)
	}
	if !changed {
		return nil
	}
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// Write-then-rename: docker may be reading this file at the same moment,
	// and a truncated config is a node that cannot pull anything.
	tmp, err := os.CreateTemp(dir, ".config.json-*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", configPath, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed
	if _, err := tmp.Write(merged); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	if err := tmp.Chmod(dockerConfigMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", configPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", configPath, err)
	}
	if err := os.Rename(tmpName, configPath); err != nil {
		return fmt.Errorf("install %s: %w", configPath, err)
	}
	return nil
}

// MergeCredHelpers adds credHelpers[host] = suffix to a docker config document.
//
// It returns the new bytes, whether anything changed, and an error only when
// the existing document is not valid JSON (rewriting a file we cannot read
// would discard whatever it held). Every other key is carried through
// untouched, including keys this code has never heard of: docker's config is
// shared state, not ours.
func MergeCredHelpers(existing []byte, host, suffix string) ([]byte, bool, error) {
	if len(bytes.TrimSpace(existing)) == 0 {
		existing = []byte("{}")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(existing, &root); err != nil {
		return nil, false, fmt.Errorf("parse docker config: %w (refusing to overwrite a file that is not JSON)", err)
	}
	if root == nil {
		root = map[string]json.RawMessage{}
	}

	helpers := map[string]string{}
	if raw, ok := root["credHelpers"]; ok {
		if err := json.Unmarshal(raw, &helpers); err != nil {
			return nil, false, fmt.Errorf("credHelpers is not an object of registry→suffix pairs: %w", err)
		}
	}
	if helpers == nil {
		helpers = map[string]string{}
	}
	if current, ok := helpers[host]; ok && current == suffix {
		return existing, false, nil
	}
	helpers[host] = suffix

	encoded, err := json.Marshal(helpers)
	if err != nil {
		return nil, false, fmt.Errorf("encode credHelpers: %w", err)
	}
	root["credHelpers"] = json.RawMessage(encoded)

	out, err := json.MarshalIndent(root, "", "\t")
	if err != nil {
		return nil, false, fmt.Errorf("encode docker config: %w", err)
	}
	return append(out, '\n'), true, nil
}
