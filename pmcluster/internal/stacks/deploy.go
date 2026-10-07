package stacks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stackdrift"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/workflow"
	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
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
		meter := otel.Meter("github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks")
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
		deployTracer = otel.Tracer("github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks")
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
	// CertResolver names the ACME resolver to attach to every exposed Traefik
	// router as traefik.http.routers.<scope>.tls.certresolver ("letsencrypt"
	// when the cluster uses ACME; empty for BYO-cert clusters — see
	// manifest.ComposeWriter.CertResolver).
	CertResolver string
	// PinNode names the default node hostname stateful services (volume
	// holders) with no explicit placement are pinned to — the platform_node
	// cluster setting. Empty disables the auto-pin (see
	// manifest.ComposeWriter.PinNode).
	PinNode string
	// Pins resolves the storage placement for stateful services: per-stack
	// pins (stack move) outrank the storage_nodes round-robin, which outranks
	// PinNode. Nil keeps today's behaviour (PinNode only). Applied to the IR
	// before every render.
	Pins *PinResolver
	// BackupDir is the host archive root the on-node offen backup agent
	// writes to (mounted as /archive inside the agent). Used by Move to
	// locate the newest whole-disk archive after triggering a backup.
	// Empty falls back to backups.DefaultArchiveDir.
	BackupDir string
	// S3 is the offsite object-store destination. Used by Move-with-S3 (the
	// storage failover path) to fetch the newest archive when the source
	// storage node is down and its local archive is unreachable. Empty
	// disables FromS3 moves.
	S3 backups.S3Config
	// MkdirAll creates host directories for the volume-root bind targets
	// before deploy (nil = os.MkdirAll). Overridable in tests.
	MkdirAll func(string, os.FileMode) error
	// Stdout receives workflow step markers (▶ ...) for the deploy pipeline.
	// Nil disables the output.
	Stdout io.Writer
	// Log is the structured (zerolog) logger for deploy diagnostics. The
	// ordered-level markers and the per-level compose YAML are emitted at
	// debug level, so they reach the console, the daily audit file and
	// OpenObserve (via the logger's OTel writer) only when the configured
	// log_level is "debug". The zero value disables structured deploy
	// logging (tests, legacy callers).
	Log zerolog.Logger
}

// Deploy runs the full deploy pipeline synchronously: parse, validate,
// translate, record the revision, and apply to the swarm (ordered levels),
// waiting for every level to become healthy before returning.
func (s *Service) Deploy(ctx context.Context, p Payload) (*Result, error) {
	return s.deploy(ctx, p, false)
}

// secretExternalName maps a logical secret name to the actual Docker swarm
// secret to mount: the content-addressed <name>_<sha8> name derived from the
// DB row's Hash. Rotated secrets can never replace the immutable in-use swarm
// secret, so the compose writer references the NEW content-addressed object
// while the container mount path stays /run/secrets/<name> (BUG-007). Falls
// back to the plain name when the store has no row (unmanaged external
// secrets) or on lookup errors — a broken reference must surface at deploy
// time, not be silently mapped away.
func (s *Service) secretExternalName(ctx context.Context, name string) string {
	if s.Store == nil {
		return name
	}
	row, err := s.Store.GetSecret(ctx, name)
	if err != nil {
		return name
	}
	return store.SwarmSecretName(name, row.Hash)
}

// configExternalName maps a logical config name to the actual Docker swarm
// config to mount for a config_path() file mount: the content-addressed
// <name>_<sha8> name derived from the DB row's Hash. The config file is
// materialized in the swarm when the config is created/edited/rolled back
// (CLI + console), so the compose writer's top-level `configs:` block can
// reference the versioned object while the container path stays the logical
// target. Falls back to the plain name when the store has no row or on lookup
// errors.
func (s *Service) configExternalName(ctx context.Context, name string) string {
	if s.Store == nil {
		return name
	}
	row, err := s.Store.GetConfig(ctx, name)
	if err != nil {
		return name
	}
	return store.SwarmConfigName(name, row.Hash)
}

// mkWriter builds a ComposeWriter wired to this Service's render inputs.
// extra labels (the rendered-hash label — see runtime.RenderedHashLabel) are
// stamped on every service's deploy labels after the standard/platform labels.
func (s *Service) mkWriter(extra map[string]string) *manifest.ComposeWriter {
	return &manifest.ComposeWriter{
		VolumeRoot:   s.VolumeRoot,
		CertResolver: s.CertResolver,
		PinNode:      s.PinNode,
		SecretNames:  s.secretExternalName,
		ConfigNames:  s.configExternalName,
		ExtraLabels:  extra,
	}
}

// renderStamped renders the IR twice — once label-free to derive the content
// hash, then again stamped with runtime.RenderedHashLabel = that hash — and
// returns the LABELED bytes. Deriving the label from the label-free render
// keeps it deterministic and non-circular; the deployed/stored bytes carry the
// label so drift detection can compare the live Swarm's labels against a
// fresh render (BUG-017, k8s-style: every service carries its content hash).
func (s *Service) renderStamped(ctx context.Context, ir *manifest.IR) ([]byte, error) {
	plain, err := s.mkWriter(nil).Write(ctx, ir)
	if err != nil {
		return nil, err
	}
	return s.mkWriter(map[string]string{runtime.RenderedHashLabel: stackdrift.ContentHash(plain)}).Write(ctx, ir)
}

// DeployAsync validates and records the revision synchronously, then applies
// the swarm deploy in a background goroutine (detached from the caller's
// context) so the caller can return 202 immediately. Long-running stacks
// (e.g. a cold postgres that needs minutes to become healthy) no longer hit
// the request deadline.
func (s *Service) DeployAsync(ctx context.Context, p Payload) (*Result, error) {
	return s.deploy(ctx, p, true)
}

func (s *Service) deploy(ctx context.Context, p Payload, async bool) (res *Result, retErr error) {
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
		ir       *manifest.IR
		rendered []byte
		revision int64
		steps    []string
	)

	s.Log.Info().Str("source", p.File).Msg("deploy — starting pipeline")

	wf.Add("Parsing manifest (DSL)", func(ctx context.Context) error {
		parsed, err := manifest.Parse([]byte(p.Manifest))
		if err != nil {
			return fmt.Errorf("parse manifest: %w", err)
		}
		if parsed.Platform {
			// The platform flag marks pmcluster's own stacks (infra, edge,
			// observability, backup, sso), which are rendered by the cluster
			// update pipeline from their embedded DSL manifests — never by
			// `pmcluster deploy` or the webhook. Allowing it here would let a
			// customer manifest stamp io.pmcluster.platform=true on swarm
			// services and masquerade as a platform stack.
			return fmt.Errorf("parse manifest: platform: true is reserved for pmcluster-managed platform stacks")
		}
		if p.AppName != "" {
			parsed.Name = p.AppName
		}
		if p.Version != "" {
			parsed.Version = p.Version
		}
		stackName = parsed.Name
		app = parsed
		s.Log.Info().Str("stack", app.Name).Msg("deploy — manifest parsed")
		return nil
	})
	wf.Add("Checking for app name conflicts", func(ctx context.Context) error {
		existing, err := s.Store.GetStack(ctx, app.Name)
		if err != nil {
			if errors.Is(err, store.ErrStackNotFound) {
				return nil
			}
			return fmt.Errorf("lookup existing stack: %w", err)
		}
		// Duplication guard: an app name may only be deployed from ONE source
		// repo. Two different repos claiming the same app name would silently
		// overwrite each other's services, volumes and Traefik routes, so we
		// reject it. Same-repo redeploys (webhooks, sync) pass because their
		// repo_url matches the recorded one; legacy rows with an empty repo_url
		// and deploys without provenance are allowed to avoid blocking
		// pre-provenance workflows.
		if existing.RepoURL.Valid && existing.RepoURL.String != "" && p.RepoURL != "" &&
			existing.RepoURL.String != p.RepoURL {
			return fmt.Errorf("stack %q already exists from repo %q — deploy from a different app name or use the recorded source", app.Name, existing.RepoURL.String)
		}
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
		// Build the neutral IR first so the ordered per-level deploy can
		// topologically sort services from depends_on and render each level
		// as its own compose; the full render is what gets recorded.
		built, err := manifest.BuildIR(ctx, app, s.Resolver)
		if err != nil {
			return fmt.Errorf("translate: %w", err)
		}
		if err := s.resolvePlacements(ctx, app.Name, built); err != nil {
			return fmt.Errorf("translate: %w", err)
		}
		y, err := s.renderStamped(ctx, built)
		if err != nil {
			return fmt.Errorf("translate: %w", err)
		}
		ir = built
		rendered = y
		return nil
	})
	wf.Add("Recording revision", func(ctx context.Context) error {
		next, err := s.Store.NextFreeRevision(ctx, app.Name, time.Now().Unix())
		if err != nil {
			return fmt.Errorf("assign revision: %w", err)
		}
		revision = next
		payloadJSON, _ := json.Marshal(payloadEnvelope{Payload: p, Steps: steps})
		rev := &store.StackRevision{
			StackName:    app.Name,
			Revision:     revision,
			SourceYAML:   p.Manifest,
			RenderedYAML: string(rendered),
			// RenderedHash is left EMPTY up front: the revision number,
			// source YAML and audit trail are recorded now, but the hash is
			// stamped only AFTER the swarm apply succeeds (see the deploy
			// step below). A failed deploy therefore leaves the hash empty so
			// the next Sync sees a mismatch and retries (BUG-018).
			RenderedHash: "",
			SourceFile:   p.File,
			PayloadJSON:  sql.NullString{String: string(payloadJSON), Valid: true},
		}
		if err := s.Store.RecordDeploy(ctx, rev, p.RepoURL); err != nil {
			return fmt.Errorf("record deploy: %w", err)
		}
		return nil
	})
	wf.Add("Ensuring storage directories", func(ctx context.Context) error {
		mkdirAll := s.MkdirAll
		if mkdirAll == nil {
			mkdirAll = os.MkdirAll
		}
		if err := manifest.EnsureVolumeDirs(rendered, mkdirAll); err != nil {
			return err
		}
		return nil
	})
	wf.Add("Deploying stack to the swarm", func(ctx context.Context) error {
		if async {
			// Fire-and-forget: validation, conflict checks and revision
			// recording all ran synchronously above. The swarm apply (and its
			// per-level health waits) continues in the background, detached
			// from the caller so a slow stack never hits the request deadline.
			// The apply outcome is observable via the stack's services, the
			// deploy logs, telemetry, and the stack's last_error field.
			go func() {
				applyCtx := context.WithoutCancel(ctx)
				if err := s.applyToSwarm(applyCtx, app, ir, rendered, revision); err != nil {
					s.Log.Error().Err(err).Str("stack", app.Name).
						Int64("revision", revision).Msg("deploy — background apply failed")
					_, _ = s.Store.RecordStackError(applyCtx, app.Name, revision, err.Error())
					return
				}
				// Stamp the rendered hash only now that the apply succeeded.
				// A failed apply returns above and leaves the hash empty, so
				// the next Sync sees a mismatch and retries (BUG-018).
				if err := s.Store.SetRevisionRenderedHash(applyCtx, app.Name, revision, store.ConfigHash(string(rendered))); err != nil {
					s.Log.Error().Err(err).Str("stack", app.Name).
						Int64("revision", revision).Msg("deploy — failed to stamp rendered hash after background apply")
				}
				_, _ = s.Store.RecordStackError(applyCtx, app.Name, revision, "")
			}()
			return nil
		}
		if err := s.applyToSwarm(ctx, app, ir, rendered, revision); err != nil {
			return err
		}
		// Stamp the rendered hash only now that the apply succeeded. A failed
		// apply returned above and left the hash empty, so the next Sync sees
		// a mismatch and retries (BUG-018).
		return s.Store.SetRevisionRenderedHash(ctx, app.Name, revision, store.ConfigHash(string(rendered)))
	})

	// Snapshot the step names in order (all steps are added before Run) so the
	// revision records the pipeline it ran.
	steps = wf.Steps()

	if err := wf.Run(ctx); err != nil {
		// Record the failure on the stack so the console surfaces it even when
		// the caller (e.g. a fire-and-forget webhook) already got a 202.
		// app can be nil when the manifest failed to parse — nothing was
		// recorded, so there is no stack to annotate.
		if app != nil {
			_, _ = s.Store.RecordStackError(ctx, app.Name, revision, err.Error())
		}
		return nil, err
	}
	if app != nil {
		_, _ = s.Store.RecordStackError(ctx, app.Name, revision, "")
	}

	if async {
		s.Log.Info().Str("stack", app.Name).Int64("revision", revision).
			Strs("services", serviceNames(ir)).Msg("deploy — accepted, applying in background")
	} else {
		s.Log.Info().Str("stack", app.Name).Int64("revision", revision).
			Strs("services", serviceNames(ir)).Msg("deploy — completed")
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
		telemetry.RecordReconcile(ctx, stackName, telemetry.ReconcileError)
		return nil, fmt.Errorf("load latest revision: %w", err)
	}
	if len(revs) == 0 {
		telemetry.RecordReconcile(ctx, stackName, telemetry.ReconcileError)
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
				var built *manifest.IR
				built, err = manifest.BuildIR(ctx, parsed, s.Resolver)
				if err == nil {
					err = s.resolvePlacements(ctx, stackName, built)
				}
				if err == nil {
					rendered, err = s.renderStamped(ctx, built)
				}
				if err == nil && store.ConfigHash(string(rendered)) == latest.RenderedHash && latest.RenderedHash != "" {
					// The rendered hash still matches the deployed revision.
					// Additionally check the LIVE swarm: a manual `docker
					// service update`, a half-applied deploy, or a wiped
					// service leaves the stored hash unchanged while the live
					// services no longer match the fresh render (BUG-017 —
					// k8s-style, every service carries its content hash).
					inSync, reason, syncErr := stackdrift.InSync(ctx, s.Docker, stackName, rendered)
					switch {
					case syncErr != nil:
						// A transient docker error must not cause a redeploy
						// loop — treat as in sync and continue the no-op.
						s.Log.Warn().Err(syncErr).Str("stack", stackName).
							Msg("sync — live drift check failed, assuming in sync")
					case !inSync:
						s.Log.Info().Str("stack", stackName).Str("reason", reason).
							Msg("sync — live swarm drifted, re-applying")
						// Fall through to reconcileDeploy — do NOT early-return
						// the no-op.
						return s.reconcileDeploy(ctx, stackName, latest.SourceYAML)
					default:
						// No drift: the stored manifest still renders to the
						// already-deployed hash AND the live swarm matches.
						s.Log.Info().Str("stack", stackName).Int64("revision", latest.Revision).
							Msg("sync — no drift, rendered hash unchanged (nothing to apply)")
					}
					telemetry.RecordReconcile(ctx, stackName, telemetry.ReconcileInSync)
					return &Result{
						StackName:    stackName,
						Revision:     latest.Revision,
						RenderedYAML: rendered,
					}, nil
				}
			}
		}
	}

	// Either the rendered hash drifted from the deployed revision (config()
	// or secrets() inputs changed) or re-translation failed outright (e.g.
	// an operator edit removed a referenced config) — both fall back to the
	// normal deploy path, which re-applies the stored source and surfaces
	// the precise error. The reconcile outcome counter below records which.
	return s.reconcileDeploy(ctx, stackName, latest.SourceYAML)
}

// reconcileDeploy applies one Sync's stored source through the normal deploy
// pipeline and records the pmcluster.reconcile.total outcome
// (applied|error), so OpenObserve can alert on a reconcile that keeps
// failing. Exactly one sample per reconcileDeploy call.
func (s *Service) reconcileDeploy(ctx context.Context, stackName, sourceYAML string) (*Result, error) {
	res, err := s.Deploy(ctx, Payload{
		AppName:  stackName,
		Manifest: sourceYAML,
	})
	status := telemetry.ReconcileApplied
	if err != nil {
		status = telemetry.ReconcileError
	}
	telemetry.RecordReconcile(ctx, stackName, status)
	return res, err
}

// applyToSwarm runs the ordered deploy for a translated stack: services are
// topologically sorted into depends_on levels (manifest.ServiceLevels) and
// deployed level-by-level, waiting for each level to become healthy before
// the next. A single level — the common case — uses the full-stack
// DeployStack path (deploy + drift-prune + force-update) unchanged. An
// ordered multi-level deploy deploys each level WITHOUT pruning (a partial
// compose must not remove not-yet-deployed siblings), then runs one
// drift-prune pass with the full stack compose so services dropped from the
// manifest are still removed. Startup ordering therefore lives in the
// control plane, never in a rendered artifact — docker stack deploy parses
// and ignores depends_on entirely.
func (s *Service) applyToSwarm(ctx context.Context, app *dsl.App, ir *manifest.IR, rendered []byte, revision int64) error {
	if app.BackupBeforeDeploy {
		backupErr := s.runPreDeployBackup(ctx, app.Name, revision)
		if backupErr != nil && app.StrictBackup {
			return fmt.Errorf("pre-deploy backup failed (strict_backup is set): %w", backupErr)
		}
	}

	levels, err := manifest.ServiceLevels(ir)
	if err != nil {
		return err
	}

	if len(levels) <= 1 {
		s.Log.Info().Str("app", app.Name).Strs("services", serviceNames(ir)).
			Msg("deploy stack — single level: full docker stack deploy (deploy + prune + force-update)")
		s.Log.Debug().Str("app", app.Name).Strs("services", levels[0]).
			Str("compose_yaml", string(rendered)).
			Msg("deploy stack — single level: full docker stack deploy (deploy + prune + force-update)")
		if err := s.Deployer.DeployStack(ctx, app.Name, rendered); err != nil {
			return fmt.Errorf("docker stack deploy: %w", err)
		}
	} else {
		// Compute the content hash ONCE from the FULL-stack label-free render,
		// then stamp that SAME hash on every per-level subset. A per-subset
		// hash would never match the full-stack render, so the drift check
		// would redeploy every single pass (an infinite redeploy loop).
		plain, err := s.mkWriter(nil).Write(ctx, ir)
		if err != nil {
			return fmt.Errorf("render full stack for rendered hash: %w", err)
		}
		writer := s.mkWriter(map[string]string{runtime.RenderedHashLabel: stackdrift.ContentHash(plain)})
		for i, level := range levels {
			levelYAML, err := writer.Write(ctx, ir.Subset(level))
			if err != nil {
				return fmt.Errorf("render depends_on level %d: %w", i, err)
			}
			s.Log.Info().Str("app", app.Name).Int("level", i).Strs("services", level).
				Msg("deploy stack — deploying depends_on level (per-service stack deploy, no prune)")
			s.Log.Debug().Str("app", app.Name).Int("level", i).Strs("services", level).
				Str("compose_yaml", string(levelYAML)).
				Msg("deploy stack — per-service docker stack deploy (subset compose, no prune)")
			if err := s.Deployer.DeployStackNoPrune(ctx, app.Name, levelYAML); err != nil {
				return fmt.Errorf("docker stack deploy (depends_on level %d): %w", i, err)
			}
			s.Log.Info().Str("app", app.Name).Int("level", i).Strs("services", level).
				Msg("deploy stack — waiting for level to become healthy")
			s.Log.Debug().Str("app", app.Name).Int("level", i).Strs("services", level).
				Msg("deploy stack — waiting for level to become healthy")
			if err := s.waitLevelHealthy(ctx, app.Name, level); err != nil {
				return err
			}
			s.Log.Info().Str("app", app.Name).Int("level", i).Strs("services", level).
				Msg("deploy stack — level healthy")
		}
		s.Log.Info().Str("app", app.Name).Strs("services", serviceNames(ir)).
			Msg("deploy stack — all levels healthy: one drift-prune pass with the full stack compose")
		s.Log.Debug().Str("app", app.Name).
			Str("compose_yaml", string(rendered)).
			Msg("deploy stack — all levels healthy: one drift-prune pass with the full stack compose")
		if err := s.Deployer.PruneStack(ctx, app.Name, rendered); err != nil {
			return err
		}
	}

	_ = s.Deployer.PruneStaleContainers(ctx, app.Name, "10m")
	return nil
}

// serviceNames lists the stack's service names in manifest order, used for
// the structured info-level deploy logs.
func serviceNames(ir *manifest.IR) []string {
	names := make([]string, 0, len(ir.Services))
	for _, s := range ir.Services {
		names = append(names, s.Name)
	}
	return names
}

// deployWaitTimeout and deployWaitInterval bound how long an ordered
// per-level deploy waits for a level's services to become healthy before
// proceeding. Overridable in tests.
var (
	deployWaitTimeout  = 2 * time.Minute
	deployWaitInterval = 2 * time.Second
)

// waitLevelHealthy blocks until every service in the level is ready: a
// long-running service once Replicas >= Desired, a run-once job once its
// task reaches a terminal state (completion = ready; failure = deploy
// error, surfaced loudly). A nil Docker client (tests, standalone CLI)
// skips the wait.
func (s *Service) waitLevelHealthy(ctx context.Context, stackName string, level []string) error {
	if s.Docker == nil {
		return nil
	}
	deadline := time.Now().Add(deployWaitTimeout)
	ticker := time.NewTicker(deployWaitInterval)
	defer ticker.Stop()
	for {
		ready, err := s.levelReady(ctx, stackName, level)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for depends_on level [%s] to become ready", deployWaitTimeout, strings.Join(level, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// levelReady reports whether every service in the level is healthy (or the
// level's services have not been created yet, which means not ready).
func (s *Service) levelReady(ctx context.Context, stackName string, level []string) (bool, error) {
	svcs, err := s.Docker.ServiceList(ctx)
	if err != nil {
		return false, fmt.Errorf("list services (depends_on wait): %w", err)
	}
	byName := make(map[string]runtime.Service, len(svcs))
	for _, sv := range svcs {
		byName[sv.Name] = sv
	}
	for _, name := range level {
		full := stackName + "_" + name
		sv, ok := byName[full]
		if !ok {
			return false, nil // not created yet
		}
		if sv.RunOnce {
			done, err := s.runOnceDone(ctx, full)
			if err != nil {
				return false, err
			}
			if !done {
				return false, nil
			}
			continue
		}
		if sv.Desired > 0 && sv.Replicas < sv.Desired {
			return false, nil
		}
	}
	return true, nil
}

// runOnceDone reports whether a one-shot job is done. The rule: any ACTIVE
// task (running/new/pending/starting) means the job is (re)starting — keep
// waiting, because a fresh attempt supersedes every earlier record. With no
// active task, the NEWEST terminal task decides: "complete" = ready; a
// failed/rejected task means the deploy must stop with the task's error; a
// shutdown/removed task (stopped mid-flight, never completed) is also an
// error. This ordering is why a retried job that eventually succeeded
// ([failed, complete]) is READY, while a job that only ever failed is not.
func (s *Service) runOnceDone(ctx context.Context, fullName string) (bool, error) {
	tasks, err := s.Docker.ServiceTasks(ctx, fullName)
	if err != nil {
		return false, fmt.Errorf("list tasks for %s (depends_on wait): %w", fullName, err)
	}
	if len(tasks) == 0 {
		return false, nil
	}
	for _, t := range tasks {
		switch t.State {
		case "running", "new", "pending", "starting":
			return false, nil // an attempt is in flight — never ready yet
		}
	}
	// No active task: the newest terminal attempt decides. StartedAt breaks
	// ties by TaskID so the comparison is deterministic.
	latest := tasks[0]
	for _, t := range tasks[1:] {
		if t.StartedAt > latest.StartedAt || (t.StartedAt == latest.StartedAt && t.TaskID > latest.TaskID) {
			latest = t
		}
	}
	switch latest.State {
	case "complete":
		return true, nil
	case "failed", "rejected":
		return false, fmt.Errorf("run-once job %s failed during ordered deploy", fullName)
	case "shutdown", "removed":
		return false, fmt.Errorf("run-once job %s did not complete (task state %s)", fullName, latest.State)
	default:
		return false, nil
	}
}

// payloadEnvelope is the persisted shape of payload_json since the pipeline
// steps feature: the deploy payload plus the ordered step names. Legacy
// revisions store just the payload itself (no envelope).
type payloadEnvelope struct {
	Payload Payload  `json:"payload"`
	Steps   []string `json:"steps"`
}

// decodeStoredPayload reads a stored payload_json into (payload, steps). It
// accepts both the envelope shape ({"payload":{...},"steps":[...]}) and the
// legacy shape (the raw JSON *is* the payload). An empty or unparseable value
// yields a zero payload with no steps.
func decodeStoredPayload(raw string) (Payload, []string) {
	if raw == "" {
		return Payload{}, nil
	}
	var env payloadEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err == nil && hasJSONKey(raw, "payload") {
		return env.Payload, env.Steps
	}
	var p Payload
	if err := json.Unmarshal([]byte(raw), &p); err == nil {
		return p, nil
	}
	return Payload{}, nil
}

func hasJSONKey(raw, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
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
		app      *dsl.App
		ir       *manifest.IR
		rendered []byte
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
	wf.Add("Re-translating source manifest", func(ctx context.Context) error {
		// Rollback re-translates the target revision's SOURCE manifest with
		// the CURRENT translator — it never re-deploys the stored rendered
		// YAML. The fresh translation is recorded as the new revision's
		// rendered artifact, so every execution path (deploy, sync, rollback)
		// follows today's rules (volume-root relocation, certresolver,
		// auto-pin, stop-first ...). A source that no longer validates under
		// the current DSL fails loudly here instead of silently deploying a
		// stale translation.
		parsed, err := manifest.Parse([]byte(row.SourceYAML))
		if err != nil {
			return fmt.Errorf("rollback source %d no longer parses: %w", sourceRevision, err)
		}
		parsed.Name = stackName // mirror Deploy's AppName override
		if err := manifest.Interpolate(parsed); err != nil {
			return fmt.Errorf("rollback source %d no longer interpolates: %w", sourceRevision, err)
		}
		if err := manifest.Validate(parsed); err != nil {
			return fmt.Errorf("rollback source %d no longer validates: %w", sourceRevision, err)
		}
		built, err := manifest.BuildIR(ctx, parsed, s.Resolver)
		if err != nil {
			return fmt.Errorf("rollback source %d no longer translates: %w", sourceRevision, err)
		}
		if err := s.resolvePlacements(ctx, stackName, built); err != nil {
			return fmt.Errorf("rollback source %d placement: %w", sourceRevision, err)
		}
		y, err := s.renderStamped(ctx, built)
		if err != nil {
			return fmt.Errorf("rollback source %d no longer renders: %w", sourceRevision, err)
		}
		app = parsed
		ir = built
		rendered = y
		return nil
	})
	wf.Add("Recording rollback revision", func(ctx context.Context) error {
		next, err := s.Store.NextFreeRevision(ctx, stackName, time.Now().Unix())
		if err != nil {
			return fmt.Errorf("assign revision: %w", err)
		}
		revision = next
		srcPayload, _ := decodeStoredPayload(row.PayloadJSON.String)
		rolledBackPayload, _ := json.Marshal(map[string]any{
			"rollback_of": sourceRevision,
			"original":    srcPayload,
		})
		rev := &store.StackRevision{
			StackName:    stackName,
			Revision:     revision,
			SourceYAML:   row.SourceYAML,
			RenderedYAML: string(rendered),
			// RenderedHash is left EMPTY up front and stamped only after the
			// re-deploy below succeeds, matching Deploy/Sync (BUG-018).
			RenderedHash: "",
			PayloadJSON:  sql.NullString{String: string(rolledBackPayload), Valid: true},
		}
		if err := s.Store.RecordDeploy(ctx, rev, ""); err != nil {
			return fmt.Errorf("record rollback: %w", err)
		}
		return nil
	})
	wf.Add("Re-deploying stack from source", func(ctx context.Context) error {
		if err := s.applyToSwarm(ctx, app, ir, rendered, revision); err != nil {
			return err
		}
		// Stamp the rendered hash only now that the apply succeeded (BUG-018).
		return s.Store.SetRevisionRenderedHash(ctx, stackName, revision, store.ConfigHash(string(rendered)))
	})

	if err := wf.Run(ctx); err != nil {
		return nil, err
	}

	s.Log.Info().Str("stack", stackName).Int64("revision", revision).
		Int64("rollback_of", sourceRevision).
		Strs("services", serviceNames(ir)).Msg("rollback — completed")

	return &Result{
		StackName:    stackName,
		Revision:     revision,
		RenderedYAML: rendered,
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

// runPreDeployBackup records its outcome in the audit table. Returns an
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
