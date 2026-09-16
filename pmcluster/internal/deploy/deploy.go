// Package deploy is the shared engine behind the REST, webhook, and CLI
// deploy paths. Pipeline:
//
//	Payload → Parse → (override version) → Interpolate → Validate
//	        → Translate → RecordDeploy → DeployStack
package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backup"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Instruments are lazily-initialised so importing this package never
// touches the global MeterProvider before telemetry.Init has had a
// chance to register a real one (otherwise we'd cache the noop meter).
var (
	instrOnce        sync.Once
	deploysTotal     metric.Int64Counter
	deployDurationMs metric.Float64Histogram
	deployTracer     trace.Tracer
)

func instruments() (metric.Int64Counter, metric.Float64Histogram, trace.Tracer) {
	instrOnce.Do(func() {
		meter := otel.Meter("github.com/hazemarian/poor-man-stack/pmcluster/internal/deploy")
		var err error
		deploysTotal, err = meter.Int64Counter(
			"pmcluster.deploys.total",
			metric.WithDescription("Total number of stack deploys, labelled by stack and status"),
		)
		if err != nil {
			deploysTotal, _ = otel.Meter("noop").Int64Counter("noop")
		}
		deployDurationMs, err = meter.Float64Histogram(
			"pmcluster.deploy.duration",
			metric.WithUnit("ms"),
			metric.WithDescription("Wall-clock duration of a stack deploy, labelled by stack and status"),
		)
		if err != nil {
			deployDurationMs, _ = otel.Meter("noop").Float64Histogram("noop")
		}
		deployTracer = otel.Tracer("github.com/hazemarian/poor-man-stack/pmcluster/internal/deploy")
	})
	return deploysTotal, deployDurationMs, deployTracer
}

// BackupTrigger is implemented by internal/backup; abstracted so tests
// can stub it without spinning up offen.
type BackupTrigger interface {
	Trigger(ctx context.Context) (archivePaths []string, err error)
}

// Payload is the canonical deploy request. JSON shape is shared by the
// REST handler and the webhook receiver.
type Payload struct {
	AppName  string `json:"app_name,omitempty"`
	RepoURL  string `json:"repo_url,omitempty"`
	Version  string `json:"version,omitempty"`
	Manifest string `json:"manifest"`
}

type DeployResult struct {
	StackName    string
	Revision     int64
	RenderedYAML []byte
}

// Service bundles deploy-pipeline dependencies. Backup is optional;
// manifests with backup_before_deploy still proceed when nil.
type Service struct {
	Store    *store.Store
	Deployer cluster.StackDeployer
	// Docker is used by Undeploy to find + remove the stack's Swarm secrets
	// and named volumes. Nil disables the swarm-asset cleanup (tests, CLI
	// deploy command).
	Docker docker.Client
	Backup BackupTrigger
	// Resolver resolves `env: X: config(name)` references against the
	// DB config store. Nil disables config() resolution (translate error).
	Resolver manifest.EnvResolver
}

func (s *Service) Deploy(ctx context.Context, p Payload) (res *DeployResult, retErr error) {
	counter, hist, tracer := instruments()
	ctx, span := tracer.Start(ctx, "pmcluster.deploy",
		trace.WithSpanKind(trace.SpanKindInternal),
	)
	start := time.Now()

	stackName := ""
	defer func() {
		status := "ok"
		if retErr != nil {
			status = "error"
			span.RecordError(retErr)
			span.SetStatus(codes.Error, retErr.Error())
		}
		attrs := metric.WithAttributes(
			attribute.String("stack", stackName),
			attribute.String("status", status),
		)
		counter.Add(ctx, 1, attrs)
		hist.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
		span.SetAttributes(
			attribute.String("pmcluster.stack", stackName),
			attribute.String("pmcluster.status", status),
		)
		span.End()
	}()

	if p.Manifest == "" {
		return nil, fmt.Errorf("manifest: required")
	}

	app, err := manifest.Parse([]byte(p.Manifest))
	if err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if p.AppName != "" {
		app.Name = p.AppName
	}
	if p.Version != "" {
		app.Version = p.Version
	}
	stackName = app.Name
	if err := manifest.Interpolate(app); err != nil {
		return nil, fmt.Errorf("interpolate: %w", err)
	}
	if err := manifest.Validate(app); err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}

	rendered, err := manifest.TranslateWithResolver(ctx, app, s.Resolver)
	if err != nil {
		return nil, fmt.Errorf("translate: %w", err)
	}

	revision, err := s.Store.NextFreeRevision(ctx, app.Name, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("assign revision: %w", err)
	}
	payloadJSON, _ := json.Marshal(p)
	rev := &store.StackRevision{
		StackName:    app.Name,
		Revision:     revision,
		SourceYAML:   p.Manifest,
		RenderedYAML: string(rendered),
		PayloadJSON:  sql.NullString{String: string(payloadJSON), Valid: true},
	}
	if err := s.Store.RecordDeploy(ctx, rev, p.RepoURL); err != nil {
		return nil, fmt.Errorf("record deploy: %w", err)
	}

	if app.BackupBeforeDeploy {
		backupErr := s.runPreDeployBackup(ctx, app.Name, revision)
		if backupErr != nil && app.StrictBackup {
			return nil, fmt.Errorf("pre-deploy backup failed (strict_backup is set): %w", backupErr)
		}
	}

	if err := s.Deployer.DeployStack(ctx, app.Name, rendered); err != nil {

		return nil, fmt.Errorf("docker stack deploy: %w", err)
	}

	_ = s.Deployer.PruneStaleContainers(ctx, app.Name, "10m")

	return &DeployResult{
		StackName:    app.Name,
		Revision:     revision,
		RenderedYAML: rendered,
	}, nil
}

// Rollback re-applies a stored revision as a NEW revision so the audit
// trail records both deploys. PayloadJSON carries a rollback_of marker.
func (s *Service) Rollback(ctx context.Context, stackName string, sourceRevision int64) (res *DeployResult, retErr error) {
	counter, hist, tracer := instruments()
	ctx, span := tracer.Start(ctx, "pmcluster.rollback",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("pmcluster.stack", stackName),
			attribute.Int64("pmcluster.rollback_source_revision", sourceRevision),
		),
	)
	start := time.Now()
	defer func() {
		status := "ok"
		if retErr != nil {
			status = "error"
			span.RecordError(retErr)
			span.SetStatus(codes.Error, retErr.Error())
		}
		attrs := metric.WithAttributes(
			attribute.String("stack", stackName),
			attribute.String("status", status),
			attribute.String("op", "rollback"),
		)
		counter.Add(ctx, 1, attrs)
		hist.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
		span.End()
	}()

	src, err := s.Store.GetRevision(ctx, stackName, sourceRevision)
	if err != nil {
		return nil, err
	}

	revision, err := s.Store.NextFreeRevision(ctx, stackName, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("assign revision: %w", err)
	}
	rolledBackPayload, _ := json.Marshal(map[string]any{
		"rollback_of": sourceRevision,
		"original":    json.RawMessage(src.PayloadJSON.String),
	})

	rev := &store.StackRevision{
		StackName:    stackName,
		Revision:     revision,
		SourceYAML:   src.SourceYAML,
		RenderedYAML: src.RenderedYAML,
		PayloadJSON:  sql.NullString{String: string(rolledBackPayload), Valid: true},
	}
	if err := s.Store.RecordDeploy(ctx, rev, ""); err != nil {
		return nil, fmt.Errorf("record rollback: %w", err)
	}

	if err := s.Deployer.DeployStack(ctx, stackName, []byte(src.RenderedYAML)); err != nil {
		return nil, fmt.Errorf("docker stack deploy (rollback): %w", err)
	}

	return &DeployResult{
		StackName:    stackName,
		Revision:     revision,
		RenderedYAML: []byte(src.RenderedYAML),
	}, nil
}

// Undeploy removes a deployed stack from the swarm and deletes its record
// (and, via the FK cascade, its revision history) from the store. It is the
// single implementation behind DELETE /api/stacks/{name} and the console's
// stack Delete button.
func (s *Service) Undeploy(ctx context.Context, stackName string) (retErr error) {
	counter, hist, tracer := instruments()
	ctx, span := tracer.Start(ctx, "pmcluster.undeploy",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("pmcluster.stack", stackName),
		),
	)
	start := time.Now()
	defer func() {
		status := "ok"
		if retErr != nil {
			status = "error"
			span.RecordError(retErr)
			span.SetStatus(codes.Error, retErr.Error())
		}
		attrs := metric.WithAttributes(
			attribute.String("stack", stackName),
			attribute.String("status", status),
			attribute.String("op", "undeploy"),
		)
		counter.Add(ctx, 1, attrs)
		hist.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
		span.End()
	}()

	if _, err := s.Store.GetStack(ctx, stackName); err != nil {
		return err // ErrStackNotFound → 404 for unknown stacks
	}

	// Collect the swarm assets the stack owns BEFORE removing its services:
	// the secrets its services mount and the named volumes docker stack rm
	// leaves behind (labelled com.docker.stack.namespace=<stack>).
	var volumes, secretNames []string
	if s.Docker != nil {
		var err error
		secretNames, err = s.Docker.StackSecretNames(ctx, stackName)
		if err != nil {
			return fmt.Errorf("collect stack secrets: %w", err)
		}
		volumes, err = s.Docker.VolumeList(ctx, docker.StackNamespaceLabel, stackName)
		if err != nil {
			return fmt.Errorf("collect stack volumes: %w", err)
		}
	}

	if err := s.Deployer.RemoveStack(ctx, stackName); err != nil {
		return fmt.Errorf("docker stack rm: %w", err)
	}

	if s.Docker != nil {
		for _, v := range volumes {
			_ = s.Docker.VolumeRemove(ctx, v)
		}
		for _, n := range secretNames {
			_ = s.Docker.SecretRemove(ctx, n)
		}
	}

	// Last: the DB record — stack + revisions (FK cascade) + the stack's
	// service-scope configs and secrets.
	if err := s.Store.DeleteStack(ctx, stackName); err != nil {
		return fmt.Errorf("delete stack record: %w", err)
	}
	return nil
}

// runPreDeployBackup records its outcome in the audit table.  Returns an
// error so callers can decide whether to abort the deploy.
func (s *Service) runPreDeployBackup(ctx context.Context, stackName string, revision int64) error {
	id, err := s.Store.CreateBackup(ctx, stackName, revision)
	if err != nil {
		return fmt.Errorf("create backup record: %w", err)
	}
	if s.Backup == nil {
		_ = s.Store.FinishBackup(ctx, id, "failed", "", "no BackupTrigger configured (deploy.Service.Backup is nil)")
		backup.RecordOutcome(ctx, backup.KindPreDeploy, backup.StatusFailed)
		return fmt.Errorf("no backup trigger configured")
	}
	paths, err := s.Backup.Trigger(ctx)
	if err != nil {
		_ = s.Store.FinishBackup(ctx, id, "failed", strings.Join(paths, ","), err.Error())
		backup.RecordOutcome(ctx, backup.KindPreDeploy, backup.StatusFailed)
		return fmt.Errorf("backup trigger: %w", err)
	}
	_ = s.Store.FinishBackup(ctx, id, "succeeded", strings.Join(paths, ","), "")
	backup.RecordOutcome(ctx, backup.KindPreDeploy, backup.StatusSucceeded)
	return nil
}
