package backups

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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

// Triggering a backup used to mean `docker exec` into the offen agent
// container. That agent is gone: stack data is archived by the node-local
// systemd unit (pmcluster-backup.timer), which snapshots the volume (one thin
// metadata operation — nothing is frozen), writes one artifact per stack,
// verifies them, ships them to the store, and prunes by retention.
//
// So a trigger is now "run that unit". The part worth stating: creating and
// mounting a snapshot needs root, so this refuses with the exact command to run
// instead of pretending a request was queued.

// Result carries the agent's output plus the artifact paths parsed out of it.
type Result struct {
	ArchivePaths []string
	Stdout       string
	Stderr       string
}

// WALCheckpointer is implemented by *store.Store so Trigger can flush SQLite WAL
// before a snapshot.
type WALCheckpointer interface {
	WALCheckpoint(ctx context.Context) error
}

// LocalTrigger satisfies the stacks package's BackupTrigger consumer interface
// via the package-level Trigger. If a WALCheckpointer is set, it is called
// before the run so SQLite's WAL is folded into the main database file.
type LocalTrigger struct {
	Store WALCheckpointer
}

// Trigger runs the package-level backup pipeline after flushing the SQLite WAL
// when a checkpointer is configured, returning the created archive paths.
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

// agentBinary is the executable to run. Overridable for tests and for a daemon
// installed somewhere unusual.
var agentBinary = func() string {
	if v := os.Getenv("PMCLUSTER_BINARY"); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "pmcluster"
}

// runAgent executes `pmcluster backup-agent run`. A var so tests substitute a
// fake that records the invocation.
var runAgent = func(ctx context.Context, bin string) (stdout, stderr string, err error) {
	var outBuf, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "backup-agent", "run")
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	return outBuf.String(), errBuf.String(), cmd.Run()
}

// geteuid is a seam for tests: the real check is os.Geteuid.
var geteuid = os.Geteuid

// Trigger runs one backup on THIS node and returns the artifacts it wrote.
//
// Caller should pass a context with a deadline: a run on a busy node is seconds,
// not milliseconds, and the freeze length follows the dirty-page backlog.
func Trigger(ctx context.Context) (*Result, error) {
	if geteuid() != 0 {
		return nil, fmt.Errorf("triggering a backup needs root (fsfreeze, lvcreate, mount): run `sudo pmcluster backup-agent run` on this node, or wait for the hourly timer (`systemctl list-timers pmcluster-backup.timer`)")
	}
	stdout, stderr, err := runAgent(ctx, agentBinary())
	res := &Result{ArchivePaths: parseArchivePaths(stdout), Stdout: stdout, Stderr: stderr}
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = strings.TrimSpace(stdout)
		}
		if detail == "" {
			return res, fmt.Errorf("backup-agent run: %w — it printed nothing; check `journalctl -u pmcluster-backup.service -n 50`", err)
		}
		// Short on purpose: this string ends up in an HTTP response, and the
		// agent's own journal has the long version.
		if len(detail) > 600 {
			detail = detail[len(detail)-600:]
		}
		return res, fmt.Errorf("backup-agent run: %w (%s)", err, detail)
	}
	return res, nil
}

// parseArchivePaths pulls artifact file names out of the agent's output: one
// line per stack, ending in the archive name, with sizes and verdicts around it.
func parseArchivePaths(stdout string) []string {
	var out []string
	for _, tok := range strings.Fields(stdout) {
		tok = strings.TrimRight(tok, ".,;:")
		if strings.HasPrefix(tok, "backup-") && strings.HasSuffix(tok, ".tar.gz") {
			out = append(out, tok)
		}
	}
	return out
}
