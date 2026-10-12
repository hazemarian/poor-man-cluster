package backups

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Backup kinds passed to RecordOutcome so dashboards can distinguish
// "ops triggered manually" from "deploy auto-triggered".
const (
	KindOnDemand  = "on_demand"
	KindPreDeploy = "pre_deploy"
)

// Backup statuses — same values stored in the audit table.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	// StatusOffsiteMissing marks a scheduled run whose local/store archive
	// exists but whose configured offsite copy does NOT: the run is
	// restorable from the archive on disk, but the offsite leg failed. Kept
	// distinct from succeeded so an offsite outage cannot read as a clean
	// backup history.
	StatusOffsiteMissing = "offsite-missing"
)

var (
	instrOnce      sync.Once
	backupsTotal   metric.Int64Counter
	backupLastUnix metric.Int64Gauge
)

func instruments() (metric.Int64Counter, metric.Int64Gauge) {
	instrOnce.Do(func() {
		meter := otel.Meter("github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups")
		var err error
		backupsTotal, err = meter.Int64Counter(
			"pmcluster.backups.total",
			metric.WithDescription("Backup runs counted by status and kind"),
		)
		if err != nil {
			backupsTotal, _ = otel.Meter("noop").Int64Counter("noop")
		}
		backupLastUnix, err = meter.Int64Gauge(
			"pmcluster.backup.last_unix",
			metric.WithUnit("s"),
			metric.WithDescription("Unix timestamp of the most recent backup attempt, labelled by status and kind. Stays alertable when a scheduled or pre-deploy backup hasn't run."),
		)
		if err != nil {
			backupLastUnix, _ = otel.Meter("noop").Int64Gauge("noop")
		}
	})
	return backupsTotal, backupLastUnix
}

// RecordOutcome bumps the backup counter + gauge. Safe to call from any
// trigger site — keeps deploy/cli/api in agreement about what's observed.
func RecordOutcome(ctx context.Context, kind, status string) {
	counter, gauge := instruments()
	attrs := metric.WithAttributes(
		attribute.String("kind", kind),
		attribute.String("status", status),
	)
	counter.Add(ctx, 1, attrs)
	gauge.Record(ctx, time.Now().Unix(), attrs)
}

// ServiceName matches the bundled backup-stack.yml (<stack>_<service>).
const ServiceName = "backup_volume-backup"

// WALCheckpointer is implemented by *store.Store so Trigger can flush
// SQLite WAL before taking a volume snapshot.
type WALCheckpointer interface {
	WALCheckpoint(ctx context.Context) error
}

// LocalTrigger satisfies the stacks package's BackupTrigger consumer
// interface via the package-level Trigger. If a WALCheckpointer is set, it
// is called before the backup exec to flush pending transactions from the
// WAL to the main DB file.
type LocalTrigger struct {
	Store WALCheckpointer
}

// Trigger runs the package-level backup pipeline after flushing the SQLite
// WAL when a checkpointer is configured, returning the created archive paths.
func (lt LocalTrigger) Trigger(ctx context.Context) ([]string, error) {

	if lt.Store != nil {
		if err := lt.Store.WALCheckpoint(ctx); err != nil {
			return nil, fmt.Errorf("wal checkpoint before backup: %w", err)
		}
	}
	res, err := Trigger(ctx)
	if err != nil {
		if res != nil {
			return res.ArchivePaths, err
		}
		return nil, err
	}
	return res.ArchivePaths, nil
}

// Result carries the stdout/stderr of a backup exec plus the archive paths
// parsed out of it.
type Result struct {
	ArchivePaths []string
	Stdout       string
	Stderr       string
}

// Trigger runs `docker exec <offen> backup`. Caller should pass a ctx
// with a deadline (~5 min is plenty for typical volumes).
func Trigger(ctx context.Context) (*Result, error) {
	containerID, err := findLocalContainer(ctx)
	if err != nil {
		return nil, err
	}
	return triggerExec(ctx, containerID)
}

// runBackupExec executes `docker exec <container> backup`, returning stdout and
// stderr. A package var so tests can substitute a fake that fails once then
// succeeds.
var runBackupExec = func(ctx context.Context, containerID string) (stdout, stderr string, err error) {
	var outBuf, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "exec", containerID, "backup")
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	return outBuf.String(), errBuf.String(), cmd.Run()
}

// backupRetryDelay is how long Trigger waits between attempts. A var so tests
// can shorten it.
var backupRetryDelay = 10 * time.Second

// backupMaxAttempts bounds how many times the agent's `backup` command is tried.
//
// The agent accepts it roughly 30-60s after its container starts; before that
// offen exits 1 having written NOTHING — which is not distinguishable from a
// genuine failure except by trying again. One retry was not enough for a fresh
// `cluster up`, where a backup is a common next move: the command failed on a
// cluster that was seconds away from being able to take one. Six attempts cover
// a cold start, and still fail a broken agent in bounded time.
const backupMaxAttempts = 6

// triggerExec runs the offen backup exec, retrying while the agent is not yet
// accepting work, bounded by both backupMaxAttempts and the caller's context
// (the CLI's --timeout).
func triggerExec(ctx context.Context, containerID string) (*Result, error) {
	var stdout, stderr string
	var err error
	for attempt := 1; ; attempt++ {
		stdout, stderr, err = runBackupExec(ctx, containerID)
		if err == nil {
			return backupResult(stdout, stderr), nil
		}
		if attempt >= backupMaxAttempts || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			// The deadline arrived while we would have waited — report the
			// failure rather than blocking past it.
			return &Result{Stdout: stdout, Stderr: stderr}, backupExecError(ctx, containerID, err, stderr)
		case <-time.After(backupRetryDelay):
		}
	}
	return &Result{Stdout: stdout, Stderr: stderr}, backupExecError(ctx, containerID, err, stderr)
}

func backupResult(stdout, stderr string) *Result {
	return &Result{
		ArchivePaths: parseArchivePaths(stdout),
		Stdout:       stdout,
		Stderr:       stderr,
	}
}

// agentLogTail returns the agent container's most recent log lines, or "" when
// they cannot be read. A var so tests can substitute one.
//
// offen writes to its OWN container streams, which `docker exec` does not
// capture — so this is the only place its reason for failing is visible.
var agentLogTail = func(ctx context.Context, containerID string, n int) string {
	var out, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", strconv.Itoa(n), containerID)
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return ""
	}
	tail := strings.TrimSpace(strings.TrimSpace(out.String()) + "\n" + strings.TrimSpace(errBuf.String()))
	if len(tail) > 600 {
		tail = tail[len(tail)-600:]
	}
	return tail
}

// backupExecError describes a failed backup exec usefully.
//
// A failing exec used to surface as `docker exec <id> backup: exit status 1 ()`
// — an exit code, and an empty cause, because the agent logged its reason to a
// stream the exec never captured. Attach the agent's own log. When even that is
// empty, say what to check instead of showing an empty pair of brackets.
func backupExecError(ctx context.Context, containerID string, err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		detail = agentLogTail(ctx, containerID, 15)
	}
	if detail == "" {
		return fmt.Errorf("docker exec %s backup: %w — the agent logged nothing, so it is most likely still "+
			"starting. Check `docker service ps backup_volume-backup` for restarts, and `docker logs` on it for why",
			containerID, err)
	}
	return fmt.Errorf("docker exec %s backup: %w (%s)", containerID, err, detail)
}

// findLocalContainer filters by service-name label so a renamed or
// replaced container is still found by its provenance.
func findLocalContainer(ctx context.Context) (string, error) {
	timeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(timeout, "docker", "ps",
		"--filter", "label=com.docker.swarm.service.name="+ServiceName,
		"--filter", "status=running",
		"--format", "{{.ID}}",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker ps: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	id := strings.TrimSpace(stdout.String())
	if id == "" {
		return "", errors.New("no offen backup agent on this node — it is not a storage node (the agent runs only on nodes labeled pmcluster.storage). Run this on a storage node, or make this node one: pmcluster node promote <hostname>")
	}

	if idx := strings.IndexByte(id, '\n'); idx >= 0 {
		id = id[:idx]
	}
	return id, nil
}

// parseArchivePaths is permissive — offen's wording has shifted across
// versions, so we match any /archive/*.tar.gz token in stdout.
func parseArchivePaths(stdout string) []string {
	var out []string
	for _, tok := range strings.Fields(stdout) {
		tok = strings.TrimRight(tok, ".,;:")
		if strings.HasPrefix(tok, "/archive/") && strings.HasSuffix(tok, ".tar.gz") {
			out = append(out, tok)
		}
	}
	return out
}
