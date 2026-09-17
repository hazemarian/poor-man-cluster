package backups

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
)

var (
	instrOnce      sync.Once
	backupsTotal   metric.Int64Counter
	backupLastUnix metric.Int64Gauge
)

func instruments() (metric.Int64Counter, metric.Int64Gauge) {
	instrOnce.Do(func() {
		meter := otel.Meter("github.com/hazemarian/poor-man-stack/pmcluster/internal/backups")
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

// LocalTrigger satisfies the deploy package's BackupTrigger consumer
// interface via the package-level Trigger. If a WALCheckpointer is set, it
// is called before the backup exec to flush pending transactions from the
// WAL to the main DB file.
type LocalTrigger struct {
	Store WALCheckpointer
}

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

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "exec", containerID, "backup")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &Result{Stdout: stdout.String(), Stderr: stderr.String()},
			fmt.Errorf("docker exec %s backup: %w (%s)", containerID, err, strings.TrimSpace(stderr.String()))
	}
	return &Result{
		ArchivePaths: parseArchivePaths(stdout.String()),
		Stdout:       stdout.String(),
		Stderr:       stderr.String(),
	}, nil
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
		return "", errors.New("no offen backup container running on this host (is the backup stack deployed?)")
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
