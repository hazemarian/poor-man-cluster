package stacks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/workflow"
	"github.com/hazemarian/poor-man-stack/pmcluster/pkg/dsl"
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
		meter := otel.Meter("github.com/hazemarian/poor-man-stack/pmcluster/internal/stacks")
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
		deployTracer = otel.Tracer("github.com/hazemarian/poor-man-stack/pmcluster/internal/stacks")
	})
	return deploysTotal, deployDurationMs, deployTracer
}

// BackupTrigger is implemented by internal/backups; abstracted so tests
// can stub it without spinning up offen.
type BackupTrigger interface {
	Trigger(ctx context.Context) (archivePaths []string, err error)
}

// Service bundles deploy-pipeline dependencies. Backup is optional;
// manifests with backup_before_deploy still proceed when nil.
type Service struct {
	Store    *store.Store
	Deployer cluster.StackDeployer
	// Docker is used by Undeploy to find + remove the stack's Swarm secrets
	// and named volumes. Nil disables the swarm-asset cleanup (tests, CLI
	// deploy command).
	Docker runtime.Client
	Backup BackupTrigger
	// Resolver resolves `env: X: config(name)` references against the
	// DB config store. Nil disables config() resolution (translate error).
	Resolver manifest.EnvResolver
	// VolumeRoot forces every container volume under this host directory
	// (subpaths /<app>/<name>). Empty falls back to
	// manifest.DefaultVolumeRoot (/var/stack/data). Applied by the writer.
	VolumeRoot string
	// Stdout receives workflow step markers (▶ ...) for the deploy pipeline.
	// Nil disables the output.
	Stdout io.Writer
}

func (s *Service) Deploy(ctx context.Context, p Payload) (res *Result, retErr error) {
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

	out := io.Discard
	if s.Stdout != nil {
		out = s.Stdout
	}
	wf := workflow.NewWorkflow(out)

	var (
		app      *dsl.App
		rendered []byte
		revision int64
	)

	wf.Add("Parsing manifest (DSL)", func(ctx context.Context) error {
		parsed, err := manifest.Parse([]byte(p.Manifest))
		if err != nil {
			return fmt.Errorf("parse manifest: %w", err)
		}
		if p.AppName != "" {
			parsed.Name = p.AppName
		}
		if p.Version != "" {
			parsed.Version = p.Version
		}
		stackName = parsed.Name
		app = parsed
		return nil
	})
	wf.Add("Interpolating and validating manifest", func(ctx context.Context) error {
		if err := manifest.Interpolate(app); err != nil {
			return fmt.Errorf("interpolate: %w", err)
		}
		if err := manifest.Validate(app); err != nil {
			return fmt.Errorf("validate: %w", err)
		}
		return nil
	})
	wf.Add("Translating to Compose (resolving configs/secrets)", func(ctx context.Context) error {
		y, err := manifest.TranslateIR(ctx, app, s.Resolver, &manifest.ComposeWriter{VolumeRoot: s.VolumeRoot})
		if err != nil {
			return fmt.Errorf("translate: %w", err)
		}
		rendered = y
		return nil
	})
	wf.Add("Recording revision", func(ctx context.Context) error {
		next, err := s.Store.NextFreeRevision(ctx, app.Name, time.Now().Unix())
		if err != nil {
			return fmt.Errorf("assign revision: %w", err)
		}
		revision = next
		payloadJSON, _ := json.Marshal(p)
		rev := &store.StackRevision{
			StackName:    app.Name,
			Revision:     revision,
			SourceYAML:   p.Manifest,
			RenderedYAML: string(rendered),
			RenderedHash: store.ConfigHash(string(rendered)),
			SourceFile:   p.File,
			PayloadJSON:  sql.NullString{String: string(payloadJSON), Valid: true},
		}
		if err := s.Store.RecordDeploy(ctx, rev, p.RepoURL); err != nil {
			return fmt.Errorf("record deploy: %w", err)
		}
		return nil
	})
	wf.Add("Deploying stack to the swarm", func(ctx context.Context) error {
		if app.BackupBeforeDeploy {
			backupErr := s.runPreDeployBackup(ctx, app.Name, revision)
			if backupErr != nil && app.StrictBackup {
				return fmt.Errorf("pre-deploy backup failed (strict_backup is set): %w", backupErr)
			}
		}
		if err := s.Deployer.DeployStack(ctx, app.Name, rendered); err != nil {
			return fmt.Errorf("docker stack deploy: %w", err)
		}
		_ = s.Deployer.PruneStaleContainers(ctx, app.Name, "10m")
		return nil
	})

	if err := wf.Run(ctx); err != nil {
		return nil, err
	}

	return &Result{
		StackName:    app.Name,
		Revision:     revision,
		RenderedYAML: rendered,
		Changed:      true,
	}, nil
}

// Rollback re-applies a stored revision as a NEW revision so the audit
// trail records both deploys. PayloadJSON carries a rollback_of marker.
// Sync re-runs the deploy pipeline for an existing stack from its latest
// stored source manifest. Config()/secrets() references are resolved again
// against the DB, so edits to a stack's configs are applied to the running
// stack. This is the k8s-style reconcile: applying the stored desired state
// after the underlying inputs changed. It records a new revision.
func (s *Service) Sync(ctx context.Context, stackName string) (*Result, error) {
	revs, err := s.Store.ListRevisions(ctx, stackName, 1)
	if err != nil {
		return nil, fmt.Errorf("load latest revision: %w", err)
	}
	if len(revs) == 0 {
		return nil, fmt.Errorf("stack %q has no revisions — deploy it first", stackName)
	}
	latest := revs[0]

	// Re-translate the stored source manifest. If the rendered compose is
	// byte-identical to the latest stored revision (rendered_hash), there is
	// nothing to apply — config()/secrets() edits in the DB did not change
	// the output, so no new revision and no Docker call.
	parsed, err := manifest.Parse([]byte(latest.SourceYAML))
	if err == nil {
		parsed.Name = stackName // mirror Deploy's AppName override
		if err = manifest.Interpolate(parsed); err == nil {
			if err = manifest.Validate(parsed); err == nil {
				var rendered []byte
				rendered, err = manifest.TranslateIR(ctx, parsed, s.Resolver, &manifest.ComposeWriter{VolumeRoot: s.VolumeRoot})
				if err == nil && store.ConfigHash(string(rendered)) == latest.RenderedHash && latest.RenderedHash != "" {
					return &Result{
						StackName:    stackName,
						Revision:     latest.Revision,
						RenderedYAML: rendered,
					}, nil
				}
			}
		}
	}
	if err != nil {
		// The stored manifest failed to re-translate (e.g. an operator edit
		// removed a config the manifest references). Fall back to the normal
		// deploy path so the pipeline surfaces the precise error.
		return s.Deploy(ctx, Payload{
			AppName:  stackName,
			Manifest: latest.SourceYAML,
		})
	}

	return s.Deploy(ctx, Payload{
		AppName:  stackName,
		Manifest: latest.SourceYAML,
	})
}

func (s *Service) Rollback(ctx context.Context, stackName string, sourceRevision int64) (res *Result, retErr error) {
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

	out := io.Discard
	if s.Stdout != nil {
		out = s.Stdout
	}
	wf := workflow.NewWorkflow(out)

	var (
		row      *store.StackRevision
		revision int64
	)

	wf.Add("Loading source revision", func(ctx context.Context) error {
		src, err := s.Store.GetRevision(ctx, stackName, sourceRevision)
		if err != nil {
			return err
		}
		row = src
		return nil
	})
	wf.Add("Recording rollback revision", func(ctx context.Context) error {
		next, err := s.Store.NextFreeRevision(ctx, stackName, time.Now().Unix())
		if err != nil {
			return fmt.Errorf("assign revision: %w", err)
		}
		revision = next
		rolledBackPayload, _ := json.Marshal(map[string]any{
			"rollback_of": sourceRevision,
			"original":    json.RawMessage(row.PayloadJSON.String),
		})
		rev := &store.StackRevision{
			StackName:    stackName,
			Revision:     revision,
			SourceYAML:   row.SourceYAML,
			RenderedYAML: row.RenderedYAML,
			RenderedHash: row.RenderedHash,
			PayloadJSON:  sql.NullString{String: string(rolledBackPayload), Valid: true},
		}
		if err := s.Store.RecordDeploy(ctx, rev, ""); err != nil {
			return fmt.Errorf("record rollback: %w", err)
		}
		return nil
	})
	wf.Add("Re-deploying stack from stored YAML", func(ctx context.Context) error {
		if err := s.Deployer.DeployStack(ctx, stackName, []byte(row.RenderedYAML)); err != nil {
			return fmt.Errorf("docker stack deploy (rollback): %w", err)
		}
		return nil
	})

	if err := wf.Run(ctx); err != nil {
		return nil, err
	}

	return &Result{
		StackName:    stackName,
		Revision:     revision,
		RenderedYAML: []byte(row.RenderedYAML),
		Changed:      true,
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
		return err // ErrStackNotFound → 404 for unknown stacks (before any swarm mutation)
	}

	out := io.Discard
	if s.Stdout != nil {
		out = s.Stdout
	}
	wf := workflow.NewWorkflow(out)

	var volumes, secretNames []string

	wf.Add("Collecting stack secrets and volumes", func(ctx context.Context) error {
		if s.Docker == nil {
			return nil
		}
		names, err := s.Docker.StackSecretNames(ctx, stackName)
		if err != nil {
			return fmt.Errorf("collect stack secrets: %w", err)
		}
		vs, err := s.Docker.VolumeList(ctx, runtime.StackNamespaceLabel, stackName)
		if err != nil {
			return fmt.Errorf("collect stack volumes: %w", err)
		}
		secretNames, volumes = names, vs
		return nil
	})
	wf.Add("Removing stack services and swarm assets", func(ctx context.Context) error {
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
		return nil
	})
	wf.Add("Deleting stack record", func(ctx context.Context) error {
		if err := s.Store.DeleteStack(ctx, stackName); err != nil {
			return fmt.Errorf("delete stack record: %w", err)
		}
		return nil
	})

	if err := wf.Run(ctx); err != nil {
		return err
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
		_ = s.Store.FinishBackup(ctx, id, "failed", "", "no BackupTrigger configured (stacks.Service.Backup is nil)")
		backups.RecordOutcome(ctx, backups.KindPreDeploy, backups.StatusFailed)
		return fmt.Errorf("no backup trigger configured")
	}
	paths, err := s.Backup.Trigger(ctx)
	if err != nil {
		_ = s.Store.FinishBackup(ctx, id, "failed", strings.Join(paths, ","), err.Error())
		backups.RecordOutcome(ctx, backups.KindPreDeploy, backups.StatusFailed)
		return fmt.Errorf("backup trigger: %w", err)
	}
	_ = s.Store.FinishBackup(ctx, id, "succeeded", strings.Join(paths, ","), "")
	backups.RecordOutcome(ctx, backups.KindPreDeploy, backups.StatusSucceeded)
	return nil
}
