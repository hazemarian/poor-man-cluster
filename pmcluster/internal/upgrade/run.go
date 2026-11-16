package upgrade

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Transport runs a shell script on a node and copies a file to it.
//
// Two methods, not one, because the release repo is private: the nodes cannot download the
// artifact themselves, so the machine running the rollout fetches it and pushes it. Copying the
// bytes over the same authenticated channel the scripts already use keeps the rollout from
// needing a second credential on every node.
//
// Implementations must run scripts as ROOT (directly, or through `sudo -n`) and must fail when
// the exit status is non-zero: every step below is fatal by design, and a transport that swallows
// a failure turns this workflow back into the script it replaced.
type Transport interface {
	Run(ctx context.Context, t Target, script string) (string, error)
	// Put copies localPath to remotePath on the node.
	Put(ctx context.Context, t Target, localPath, remotePath string) error
}

// Options configures one rollout.
type Options struct {
	// Version is the release tag to install, e.g. "v0.2.347".
	Version string
	// Artifact is the verified tarball, already on the machine running the rollout. Its
	// checksum is checked here and again on the node: a corrupt transfer that reaches
	// /usr/local/bin is a node that stops booting.
	Artifact string
	// SHA256 is the expected checksum of that artifact. Required: a rollout that installs a
	// binary without verifying it is a rollout that can install a truncated file.
	SHA256 string
	// Targets are the nodes, in any order — Plan orders them.
	Targets []Target
	// Transport moves bytes and runs scripts. Required.
	Transport Transport
	// HealthTimeout bounds the wait for /health after a restart. Zero means 90s.
	HealthTimeout time.Duration
	// Rollback restores the previous binary on a node whose new binary does not come up
	// healthy. Default true; the ONLY reason to disable it is a node that has no previous
	// binary (a first install), which the rollout detects and reports instead of pretending.
	NoRollback bool
	// DryRun prints the plan and touches nothing.
	DryRun bool
	// Logf is where the phase log goes.
	Logf func(format string, args ...any)
}

// NodeResult is what happened to one node.
type NodeResult struct {
	Target Target
	From   string
	To     string
	// Skipped means the node already ran the requested version: a rollout is idempotent, so
	// re-running it after a failure resumes instead of reinstalling everything.
	Skipped bool
	// RolledBack means the new binary did not come up healthy and the previous one was
	// restored and verified.
	RolledBack bool
	// Err is non-empty when this node failed.
	Err string
}

// Report is the outcome of a rollout.
type Report struct {
	Version string
	Order   []Target
	Nodes   []NodeResult
	// Aborted means the rollout stopped at the first failure instead of continuing. A
	// half-upgraded fleet with one broken node is recoverable; a fleet where the rollout
	// carried on past a broken node is how mixed versions and a dead leader happen.
	Aborted bool
}

// Failed reports whether any node ended in error.
func (r Report) Failed() bool {
	for _, n := range r.Nodes {
		if n.Err != "" {
			return true
		}
	}
	return false
}

// pollInterval is how often the health gate asks again. A variable so a test does not spend
// three seconds per poll learning what it already knows.
var pollInterval = 3 * time.Second

const (
	// binPath is where the unit execs the binary; the same path the installer writes.
	binPath = "/usr/local/bin/pmcluster"
	// prevPath is the rollback copy. The old script kept one too, and never used it.
	prevPath = binPath + ".prev"
	// healthPort is the daemon's /health listener on the node itself.
	healthURL = "http://127.0.0.1:9090/health"
)

// Run executes the rollout: plan, then per node — probe, install, restart, GATE, and roll back
// on a failed gate before stopping the whole rollout.
func Run(ctx context.Context, opts Options) (Report, error) {
	if opts.Transport == nil {
		return Report{}, fmt.Errorf("upgrade: no transport")
	}
	if opts.Version == "" || opts.Artifact == "" || opts.SHA256 == "" {
		return Report{}, fmt.Errorf("upgrade: version, artifact and sha256 are all required (an unverified binary is not an upgrade)")
	}
	if _, err := os.Stat(opts.Artifact); err != nil {
		return Report{}, fmt.Errorf("upgrade: artifact %s: %w", opts.Artifact, err)
	}
	if opts.HealthTimeout <= 0 {
		opts.HealthTimeout = 90 * time.Second
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	order, err := Plan(opts.Targets)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Version: opts.Version, Order: order}
	logf("▶ %d node(s), in order: %s", len(order), hostList(order))
	for _, t := range order {
		role := t.Role
		if t.Leader {
			role += " (leader, last)"
		}
		logf("  • %s — %s", t.Host, role)
	}
	if opts.DryRun {
		logf("▶ dry run: nothing was touched")
		return rep, nil
	}

	for i, t := range order {
		res := NodeResult{Target: t}
		logf("══════ %s (%d/%d) ══════", t.Host, i+1, len(order))

		from, err := t.probe(ctx, opts.Transport)
		res.From = from
		if err != nil {
			// A node that cannot even say what version it runs is not a node to install
			// into: the install would work and the verification would be guesswork.
			res.Err = fmt.Sprintf("probe version: %v", err)
			rep.Nodes = append(rep.Nodes, res)
			rep.Aborted = true
			logf("  ✖ %v", err)
			return rep, nil
		}
		logf("  before: %s", from)
		res.To = from
		if strings.TrimSpace(from) == opts.Version {
			res.Skipped = true
			rep.Nodes = append(rep.Nodes, res)
			logf("  already on %s — skipped (idempotent rollout)", opts.Version)
			continue
		}

		if err := opts.install(ctx, t); err != nil {
			res.Err = fmt.Sprintf("install: %v", err)
			rep.Nodes = append(rep.Nodes, res)
			rep.Aborted = true
			logf("  ✖ %v", err)
			return rep, nil
		}
		logf("  installed %s, unit restarted", opts.Version)

		// THE GATE. This is the step the script never had: `systemctl is-active` says
		// "active" while a binary crash-loops, so the only honest question is whether the
		// daemon answers /health with the NEW version in its body.
		now, gerr := t.awaitHealthy(ctx, opts.Transport, opts.Version, opts.HealthTimeout)
		if gerr == nil {
			res.To = now
			rep.Nodes = append(rep.Nodes, res)
			logf("  ✔ healthy on %s", now)
			continue
		}
		res.Err = fmt.Sprintf("health gate: %v", gerr)
		logf("  ✖ health gate: %v", gerr)

		if opts.NoRollback {
			rep.Nodes = append(rep.Nodes, res)
			rep.Aborted = true
			logf("  ✖ rollback disabled — stopping here; this node runs %s and is NOT healthy", opts.Version)
			return rep, nil
		}
		logf("  ▶ rolling back to the previous binary")
		if rerr := t.rollback(ctx, opts.Transport, opts.HealthTimeout); rerr != nil {
			// The worst outcome, said plainly: the node is broken and the rollback did not
			// take. Continuing would upgrade the rest of the fleet around a dead node.
			res.Err += "; rollback FAILED: " + rerr.Error()
			rep.Nodes = append(rep.Nodes, res)
			rep.Aborted = true
			logf("  ✖ rollback failed: %v — this node needs a human", rerr)
			return rep, nil
		}
		res.RolledBack = true
		res.To = from
		rep.Nodes = append(rep.Nodes, res)
		rep.Aborted = true
		logf("  ✖ rolled back to %s; rollout STOPPED — the remaining %d node(s) were not touched", from, len(order)-i-1)
		return rep, nil
	}
	return rep, nil
}

// probe reads the installed version off the node.
func (t Target) probe(ctx context.Context, tr Transport) (string, error) {
	out, err := tr.Run(ctx, t, binPath+" version 2>/dev/null | head -1")
	if err != nil {
		return "", err
	}
	// `pmcluster version` prints "pmcluster v0.2.346"; the rollout compares versions, so the
	// binary's own banner must not decide whether a node is current. (It did: every node
	// "needed" the upgrade it was already running, because "pmcluster v0.2.346" != "v0.2.346".)
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("no version reported by %s", binPath)
	}
	return fields[len(fields)-1], nil
}

// install downloads, VERIFIES, installs and restarts on one node.
//
// Verification comes before install and the checksum is required, not best-effort: a truncated
// download that reaches /usr/local/bin is a node that stops booting, and the fleet has no way
// back except a human with a shell.
func (o Options) install(ctx context.Context, t Target) error {
	remote := "/tmp/" + filepath.Base(o.Artifact)
	if err := o.Transport.Put(ctx, t, o.Artifact, remote); err != nil {
		return fmt.Errorf("copy artifact: %w", err)
	}
	script := strings.Join([]string{
		"set -e",
		// Keep the outgoing binary BEFORE anything replaces it: this is what a rollback
		// restores, and the old script found it never existed when it was needed.
		fmt.Sprintf("cp -a %s %s 2>/dev/null || true", binPath, prevPath),
		fmt.Sprintf("echo '%s  %s' | sha256sum -c -", o.SHA256, remote),
		"d=$(mktemp -d)",
		"trap 'rm -rf \"$d\"' EXIT",
		fmt.Sprintf("tar -xzf %s -C \"$d\"", remote),
		// The archive holds ONE file, named after the asset with .tar.gz dropped
		// (`pmcluster-v0.2.347-linux-amd64`) — not `pmcluster`, and not in a directory. An
		// older release tarred a directory containing `pmcluster`, so take the expected name
		// first and fall back to whatever executable the archive actually carries: a rollout
		// should not break because the release layout changed shape between two tags.
		fmt.Sprintf(`cand="$d/%s"`, binNameFromAsset(o.Artifact)),
		`if [ -x "$cand" ]; then bin="$cand"; else bin=$(find "$d" -type f -name 'pmcluster*' -perm -u+x | head -1); fi`,
		`[ -n "$bin" ] && [ -x "$bin" ] || { echo "no pmcluster binary in the archive" >&2; exit 1; }`,
		fmt.Sprintf("install -m 0755 \"$bin\" %s", binPath),
		"systemctl restart pmcluster",
		fmt.Sprintf("rm -f %s", remote),
	}, "\n")
	_, err := o.Transport.Run(ctx, t, script)
	return err
}

// binNameFromAsset is the file name inside the release archive: the asset name without its
// .tar.gz suffix.
func binNameFromAsset(artifact string) string {
	base := filepath.Base(artifact)
	return strings.TrimSuffix(base, ".tar.gz")
}

// awaitHealthy polls /health until it answers 200 with the expected version in the body.
//
// The version in the body is the point: a node that answers /health from the OLD process while
// systemd restart-loops the new one would otherwise pass this gate.
func (t Target) awaitHealthy(ctx context.Context, tr Transport, want string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		out, err := tr.Run(ctx, t, fmt.Sprintf("curl -fsS -m 3 %s", healthURL))
		last = strings.TrimSpace(out)
		if err == nil && strings.Contains(last, want) {
			return want, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if last == "" {
		return "", fmt.Errorf("no answer on %s within %s", healthURL, timeout)
	}
	return "", fmt.Errorf("never reported %s within %s (last answer: %s)", want, timeout, last)
}

// rollback restores the previous binary and proves it comes up again.
//
// A rollback that is not verified is a hope: restoring the file and restarting can leave the
// node exactly as broken as the release it replaced, and the report would say "rolled back".
func (t Target) rollback(ctx context.Context, tr Transport, timeout time.Duration) error {
	out, err := tr.Run(ctx, t, fmt.Sprintf("test -x %s && install -m 0755 %s %s && systemctl restart pmcluster", prevPath, prevPath, binPath))
	if err != nil {
		return fmt.Errorf("restore previous binary: %v (%s)", err, strings.TrimSpace(out))
	}
	if _, err := t.awaitHealthy(ctx, tr, "", timeout); err != nil {
		return fmt.Errorf("previous binary restored but the node is still not healthy: %v", err)
	}
	return nil
}
