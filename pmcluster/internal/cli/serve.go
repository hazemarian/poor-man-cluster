package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/apikeys"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/certs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/configs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/controlplane"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logger"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/reconcile"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/secrets"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/server"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/settings"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/usage"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/webhooks"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the pmcluster HTTP daemon (REST API + webhook receiver)",
	Long: `Starts the long-running pmcluster daemon. Listens on 0.0.0.0:9090 by
default so the edge console container can reach it via
host.docker.internal:9090 (the docker bridge gateway); Traefik (running in
the swarm) routes pmcluster.<domain> to it as well.

The data directory ($HOME/.pmcluster by default) must already be initialised
via 'pmcluster init'.`,
	RunE: runServe,
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log, logCloser, err := logger.New(logger.Options{
		LogsDir: cfg.LogsDir(),
		Level:   cfg.LogLevel,
		Console: true,
	})
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logCloser.Close() }()

	if err := logger.Sweep(cfg.LogsDir(), time.Now()); err != nil {
		log.Warn().Err(err).Msg("log sweep had issues")
	}

	telemetryShutdown, err := telemetry.Init(cmd.Context(), telemetry.Options{
		Endpoint:       cfg.OTLPEndpoint,
		ServiceName:    serviceName(),
		ServiceVersion: buildinfo.Version,
	})
	if err != nil {
		log.Warn().Err(err).Str("endpoint", cfg.OTLPEndpoint).Msg("OTel telemetry disabled")
	} else if cfg.OTLPEndpoint != "" {
		log.Info().Str("endpoint", cfg.OTLPEndpoint).Msg("OTel telemetry enabled")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetryShutdown(ctx); err != nil {
			log.Warn().Err(err).Msg("telemetry shutdown had issues")
		}
	}()

	dc, dockerErr := docker.New()
	if dockerErr != nil {
		log.Warn().Err(dockerErr).Msg("docker client init failed; /api/cluster/info disabled")
	} else {
		defer func() { _ = dc.Close() }()

		// Leader-aware daemon: on a multi-manager cluster the daemon serves
		// ONLY on the current Swarm leader; other managers stand by until
		// Swarm elects them leader (failover). Standalone/local mode returns
		// immediately.
		if err := waitForSwarmLeadership(cmd.Context(), dc, log); err != nil {
			return fmt.Errorf("standby: %w", err)
		}

		// Control-plane freshness: when this node was a standby and just
		// became leader, restore the newest control-plane snapshot if the
		// local DB is missing or older than it. The leader snapshots the
		// survivor kit into Raft-replicated Docker configs (L2); pre-L2
		// clusters fall back to the tarball archive. Shared/replicated
		// storage (LINSTOR) keeps the DB current on every manager — never
		// clobber a fresh DB.
		if _, err := ensureControlPlaneFresh(cmd.Context(), cfg, dc, log); err != nil {
			return fmt.Errorf("control-plane restore: %w", err)
		}

		// First snapshot as leader: publish the current (possibly just
		// restored) control plane so the other managers have a fresh kit
		// before any mutation happens.
		if err := (&controlplane.Kit{Docker: dc, DataDir: cfg.DataDir, Log: log}).Snapshot(cmd.Context()); err != nil {
			log.Warn().Err(err).Msg("initial control-plane snapshot had issues")
		}

		checkConfigVersions(cmd.Context(), dc, log)
	}

	// The store is opened only after the leadership + control-plane-restore
	// block: a standby node (non-leader) has no local DB yet, and on promotion
	// the restore must happen BEFORE the DB is opened so the restored control
	// plane is what gets served.
	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — run `pmcluster init` (or `pmcluster join` on a joining node)", cfg.DataDir)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	// The persisted log_level cluster setting overrides the config-file
	// default, so a console change survives daemon restarts. Invalid values
	// are ignored here (a bad setting only surfaces at save time).
	if lvl := st.GetSettingDefault(cmd.Context(), cluster.SettingLogLevel(), ""); lvl != "" {
		_ = logger.SetLevel(lvl)
	}

	if err := replayRegistryLogins(cmd.Context(), st, cfg, log); err != nil {
		log.Warn().Err(err).Msg("registry re-login had issues; private images may fail to pull")
	}

	deployer := cluster.NewDockerCLIDeployer(cmd.OutOrStdout())

	cipher, cipherErr := credentials.Open(cfg.EncryptionKeyPath())
	if cipherErr != nil {
		log.Warn().Err(cipherErr).Msg("encryption key not available; /webhook/* disabled")
	}

	deploySvc := &stacks.Service{Store: st, Deployer: deployer, Docker: dc, Backup: backups.LocalTrigger{Store: st}, Resolver: &stacks.StoreConfigResolver{Store: st, Docker: dc, Cipher: cipher}, VolumeRoot: st.GetSettingDefault(cmd.Context(), cluster.SettingVolumeRoot(), ""), CertResolver: cluster.CertResolverForMode(st.GetSettingDefault(cmd.Context(), cluster.SettingTLSMode(), "")), PinNode: st.GetSettingDefault(cmd.Context(), cluster.SettingPlatformNode(), ""), Pins: &stacks.PinResolver{PlatformNode: st.GetSettingDefault(cmd.Context(), cluster.SettingPlatformNode(), ""), StorageNodes: stacks.ParseStorageNodes(st.GetSettingDefault(cmd.Context(), cluster.SettingStorageNodes(), "")), StackPin: func(ctx context.Context, stackName string) (string, error) {
		return st.GetSettingDefault(ctx, stacks.StackPinKey(stackName), ""), nil
	}}, Log: log, BackupDir: cluster.BackupRootDir()}

	tlsSvc := certs.NewLocal(st, cipher, dc, deployer,
		cfg.ConfigDir(), buildinfo.Version)

	deps := server.Deps{
		Lookup:        st,
		Docker:        dc,
		Store:         st,
		DeployService: deploySvc,
		Cipher:        cipher,
		Backups:       &backups.Local{Store: st, Run: backups.LocalTrigger{Store: st}.Trigger, ArchiveDir: backups.DefaultArchiveDir, RetentionDays: cluster.LoadBackupRetentionDays(cmd.Context(), st), S3: backupS3FromSettings(cmd.Context(), st)},
		Settings: func() *settings.Local {
			l := settings.NewLocal(st)
			l.ApplyLogLevel = logger.SetLevel
			l.ApplyStorageNodes = func(v string) error {
				deploySvc.Pins.SetStorageNodes(stacks.ParseStorageNodes(v))
				return nil
			}
			return l
		}(),
		Usage:     usage.NewLocal(st),
		HostCerts: &certs.HTTP{Svc: tlsSvc},
		SiteCert:  &certs.HTTP{Svc: tlsSvc},
		Update: &server.UpdateService{
			Update: func(ctx context.Context) (*cluster.UpdateResult, error) {
				if cipher == nil {
					return nil, fmt.Errorf("encryption key unavailable; cannot run cluster update")
				}
				return cluster.NewService().Update(ctx, cluster.UpdateDeps{
					Store:    st,
					Cipher:   cipher,
					Docker:   dc,
					Deployer: deployer,
					Stdout:   io.Discard,
				}, cluster.UpdateInput{ConfigDir: cfg.ConfigDir(), Version: buildinfo.Version})
			},
		},
		Configs:        &configs.Local{Store: st, Docker: dc},
		Webhooks:       webhooks.NewLocal(st, cipher),
		WebhookSources: webhooks.NewLocal(st, cipher),
		APIKeys:        apikeys.NewLocal(st),
		Services:       &services.Local{Docker: dc},
	}
	if cipher != nil {
		deps.Secrets = secrets.NewLocal(st, cipher)
	}
	handler := server.New(deps)

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Leader-only control loop: on becoming Swarm leader the daemon runs the
	// reconcile loop (platform configs + app-stack drift + health snapshot),
	// the control-plane Raft snapshot loop, and the control-plane freshness
	// restore; on losing leadership the loops stop. Non-manager/standalone
	// nodes serve without the loops.
	leadership := WatchSwarmLeadership(ctx, dc, log)
	kit := &controlplane.Kit{Docker: dc, DataDir: cfg.DataDir, Log: log}
	reconciler := &reconcile.Reconciler{
		Store:         st,
		Docker:        dc,
		DeployService: deploySvc,
		Services:      &services.Local{Docker: dc},
		Update: func(ctx context.Context, deps cluster.UpdateDeps, in cluster.UpdateInput) (*cluster.UpdateResult, error) {
			if cipher == nil {
				return nil, fmt.Errorf("encryption key unavailable; cannot reconcile platform stacks")
			}
			return cluster.NewService().Update(ctx, deps, in)
		},
		UpdateDeps: cluster.UpdateDeps{
			Store:    st,
			Cipher:   cipher,
			Docker:   dc,
			Deployer: deployer,
			Stdout:   io.Discard,
		},
		UpdateInput: cluster.UpdateInput{ConfigDir: cfg.ConfigDir(), Version: buildinfo.Version},
		Log:         log,
		Interval:    time.Duration(reconcileIntervalSeconds(cmd.Context(), st)) * time.Second,
	}
	var loopCancel context.CancelFunc
	go func() {
		for isLeader := range leadership {
			if loopCancel != nil {
				loopCancel()
				loopCancel = nil
			}
			if isLeader {
				log.Info().Msg("control loop: starting reconcile loop (leader)")
				// Publish a fresh control-plane snapshot on promotion. We do
				// NOT restore here: the local store is already open and being
				// live-written, and Restore truncates data.db under the live
				// sqlite connection (BUG-006 — split-brain: daemon serves
				// stale page-cache rows, fresh readers see the clobbered file).
				// Control-plane restore happens ONLY at startup, before the
				// store is opened (see the standby block above).
				if err := kit.Snapshot(ctx); err != nil {
					log.Warn().Err(err).Msg("control-plane snapshot on leadership gain had issues")
				}
				var loopCtx context.Context
				loopCtx, loopCancel = context.WithCancel(ctx)
				go reconciler.Loop(loopCtx)
				go kit.Loop(loopCtx, controlPlaneSnapshotInterval())
			} else {
				log.Info().Msg("control loop: stopped (not the swarm leader)")
			}
		}
	}()

	log.Info().Str("addr", cfg.ListenAddr).Msg("pmcluster serve listening")
	if err := server.Run(ctx, cfg.ListenAddr, handler); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	log.Info().Msg("pmcluster serve stopped cleanly")
	return nil
}

// pmclusterConfigBases lists the Docker config base names managed by pmcluster
// that should have a pmcluster.version label for version drift detection.
var pmclusterConfigBases = []string{"pmcluster_otel_config", "pmcluster_traefik_dynamic"}

// checkConfigVersions compares the running binary version against the
// pmcluster.version label on managed Docker configs. If any config is on an
// older version, it logs a WARN prompting 'pmcluster cluster up'.
func checkConfigVersions(ctx context.Context, dc runtime.Client, log zerolog.Logger) {
	if buildinfo.Version == "" || buildinfo.Version == "dev" {
		return
	}

	names, err := dc.ConfigList(ctx, cluster.PmclusterLabel, "true")
	if err != nil {
		log.Warn().Err(err).Msg("version check: cannot list Docker configs")
		return
	}

	seen := make(map[string]bool, len(pmclusterConfigBases))
	for _, name := range names {
		for _, base := range pmclusterConfigBases {
			if !strings.HasPrefix(name, base+"_v") {
				continue
			}
			seen[base] = true
			inspect, err := dc.ConfigInspect(ctx, name)
			if err != nil {
				log.Warn().Err(err).Str("config", name).Msg("version check: cannot inspect")
				continue
			}
			labelVer := ""
			if inspect.Labels != nil {
				labelVer = inspect.Labels["pmcluster.version"]
			}
			if labelVer == "" {

				log.Warn().
					Str("config", name).
					Str("running", buildinfo.Version).
					Msg("version check: config has no pmcluster.version label — run 'pmcluster cluster up' to regenerate")
			} else if labelVer != buildinfo.Version {
				log.Warn().
					Str("config", name).
					Str("running", buildinfo.Version).
					Str("config_version", labelVer).
					Msg("version check: config version mismatch — run 'pmcluster cluster up' to regenerate")
			}
		}
	}
	for _, base := range pmclusterConfigBases {
		if !seen[base] {
			log.Warn().Str("base", base).Msg("version check: no managed config found")
		}
	}
}

// serviceName returns the OTel resource service.name. It prefers the
// OTEL_SERVICE_NAME env var so deployments can override it, falling back to
// "pmcluster" for local runs.
func serviceName() string {
	if n := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); n != "" {
		return n
	}
	return "pmcluster"
}

// controlPlaneSnapshotInterval returns how often the leader re-checks the
// control-plane survivor kit for changes and snapshots it into Raft-replicated
// Docker configs. Defaults to controlplane.DefaultSnapshotInterval; the
// PMCLUSTER_CONTROLPLANE_SNAPSHOT_INTERVAL env var (seconds) overrides it
// (tests use 1 to converge in seconds; 0 or negative falls back to default).
func controlPlaneSnapshotInterval() time.Duration {
	raw := os.Getenv("PMCLUSTER_CONTROLPLANE_SNAPSHOT_INTERVAL")
	if raw == "" {
		return controlplane.DefaultSnapshotInterval
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return controlplane.DefaultSnapshotInterval
	}
	return time.Duration(n) * time.Second
}

// reconcileIntervalSeconds reads the persisted reconcile_interval setting
// (seconds, 0 = the loop is disabled) with a default of 60.
func reconcileIntervalSeconds(ctx context.Context, st *store.Store) int {
	const def = 60
	if st == nil {
		return def
	}
	raw := st.GetSettingDefault(ctx, cluster.SettingReconcileInterval(), "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return n
}
