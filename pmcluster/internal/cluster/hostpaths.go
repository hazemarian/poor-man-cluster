package cluster

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Host paths the platform's own stacks bind-mount from the node.
//
// Docker refuses to start a task whose bind SOURCE does not exist on the node
// it lands on:
//
//	Rejected  invalid mount config for type "bind": bind source path does not
//	exist: /var/stack/nodestat
//
// and a `docker stack deploy` that hits it leaves the service at 0/1 with no
// task logs — the exact shape of tonight's failure on the four nodes that had
// never run the node-stats code, where the directory was created by hand.
// Provisioning a host directory a platform stack mounts is the platform's job,
// not the operator's, so `cluster up` and `cluster update` both create them
// before the stack that needs them is deployed.
//
// The path lives here as ONE constant because two writers read it: the
// observability stack's bind mount (embeds/observability-stack.yml, rendered
// through RenderInput.NodeStatDir) and the collector config's file exporter
// path. A literal in either file could drift from the mkdir and the drift is
// silent — the task is rejected, or the file is written somewhere the mount
// does not cover. TestRenderedNodeStatBindSourceIsTheEnsuredHostDir pins them
// together.

// NodeStatHostDir is the host directory the OTel collector appends the
// node-stats JSONL to. It is bind-mounted into the collector at the SAME path
// so the leader's daemon — a host process — reads exactly what the task wrote.
const NodeStatHostDir = "/var/stack/nodestat"

// NodeStatFileName is the JSONL file inside NodeStatHostDir. The collector's
// file exporter creates it; only the directory has to pre-exist.
const NodeStatFileName = "nodestat.jsonl"

// hostPathMode is the mode used for a platform host directory. 0755 because
// the collector runs as root (user "0:0") and the reading daemon runs as root
// too, while nothing else needs to write there.
const hostPathMode os.FileMode = 0o755

// NodeStatFilePath is the collector's JSONL path — NodeStatHostDir plus the
// file name, so a caller never re-spells the directory.
func NodeStatFilePath() string { return NodeStatHostDir + "/" + NodeStatFileName }

// hostMkdirAll is os.MkdirAll, indirected so tests can present a read-only or
// unwritable host without touching /var/stack.
var hostMkdirAll = os.MkdirAll

// platformHostDirs is every host directory a platform stack bind-mounts that a
// stock Docker host does not already provide, derived from the same constants
// the stack template renders.
//
// The audit of the observability stack's OTHER bind sources (2026-11-09):
//
//   - /var/lib/docker/containers, /var/run/docker.sock — Docker's own; they
//     exist wherever the engine runs (a docker that lost them is broken).
//   - /etc/localtime, /proc, /sys — stock Linux.
//   - the Parseable data/staging dirs — named volumes, not binds: the writer
//     declares them with driver_opts{device:<volume_root>/observability/<name>}
//     and the deploy pipeline creates them (manifest.EnsureVolumeDirs).
//
// So NodeStatHostDir is the only source in this class, and this is the list the
// mkdir walks. Anything added here must be a path that is NOT guaranteed on a
// stock node.
func platformHostDirs() []string {
	return []string{NodeStatHostDir}
}

// ensureHostDir creates dir when it is missing. It is idempotent and a no-op
// for an existing directory (including its mode — an operator's 0700 is not
// rewritten), and it returns an error that names the path and the reason it
// could not be provided, which is what an operator needs when the host is
// read-only or the path is owned by somebody else.
func ensureHostDir(dir string) error {
	st, err := os.Stat(dir)
	switch {
	case err == nil:
		if !st.IsDir() {
			return fmt.Errorf("%s exists but is not a directory — a platform bind mount needs a directory there", dir)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("inspect host path %s: %w", dir, err)
	}
	if err := hostMkdirAll(dir, hostPathMode); err != nil {
		return fmt.Errorf("create host path %s (a platform container bind-mounts it; Swarm rejects the task when the source is missing): %w", dir, err)
	}
	return nil
}

// ensureHostDirs runs ensureHostDir over every path and reports all failures at
// once — one bad path must not hide the rest.
func ensureHostDirs(dirs []string) error {
	var errs []error
	for _, dir := range dirs {
		if err := ensureHostDir(dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// withHostPaths fills in every host path a rendered platform template reads, so
// an empty RenderInput field can never become an empty bind source. One place,
// both render paths (compose and collector config) call it.
func withHostPaths(in RenderInput) RenderInput {
	if in.NodeStatDir == "" {
		in.NodeStatDir = NodeStatHostDir
	}
	return in
}

// ensurePlatformHostDirs is the workflow-facing seam. It is a var, like
// ensureStorageDirs, so tests drive the real helper against a t.TempDir (or
// simulate an unwritable host) instead of writing to /var/stack.
//
// No role check: the directory is created on whichever node runs the command,
// manager or not. The collector is constrained to the leader, and an unused
// empty directory on any other node is harmless — while a role check would have
// to be re-derived at three call sites and would still not cover a node that
// becomes leader later. The leader's own daemon is what reconciles, so the node
// that needs the path is the node that runs this.
var ensurePlatformHostDirs = ensurePlatformHostDirsReal

// ensurePlatformHostDirsReal is the implementation behind the seam above. It
// is a named function so a test can exercise the real list and the real helper
// without depending on whatever a previous test left in the seam.
func ensurePlatformHostDirsReal() error {
	return ensureHostDirs(platformHostDirs())
}
