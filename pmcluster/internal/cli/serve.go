package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/docs"
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
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logger"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/mcpserver"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodever"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
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

	// Self-telemetry endpoint: THIS node's swarm address, resolved and
	// dial-checked at startup (config.ResolveOTLPEndpoint). A node that cannot
	// name itself — or whose collector port does not answer — gets no exporter
	// and one line saying why, instead of an exporter aimed at a closed port.
	endpoint, telemetryOff := config.ResolveOTLPEndpoint(cmd.Context(), cfg.OTLPEndpoint, docker.NodeSelfAddr, nil)
	if telemetryOff != "" {
		log.Warn().Msg(telemetryOff)
	}

	telemetryShutdown, err := telemetry.Init(cmd.Context(), telemetry.Options{
		Endpoint:       endpoint,
		ServiceName:    serviceName(),
		ServiceVersion: buildinfo.Version,
		Failures:       telemetryFailureLogger(log),
	})
	if err != nil {
		log.Warn().Err(err).Str("endpoint", endpoint).Msg("OTel telemetry disabled")
	} else if endpoint != "" {
		log.Info().Str("endpoint", endpoint).Msg("OTel telemetry enabled")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetryShutdown(ctx); err != nil {
			log.Warn().Err(err).Msg("telemetry shutdown had issues")
		}
	}()

	// storageRoot is published once the local store is open (leader path).
	// Before that the volume repair still handles stack volumes, which carry
	// their host directory on the volume itself and need no root.
	var storageRoot atomic.Value
	storageRoot.Store("")

	dc, dockerErr := docker.New()
	// One Kit per daemon: it is the object that remembers which snapshot this
	// node published, so a leadership gain cannot re-extract this node's own
	// state. Created even when Docker is unavailable (the Kit then no-ops).
	kit := &controlplane.Kit{Docker: dc, DataDir: cfg.DataDir, Log: log}

	// Every daemon describes its OWN node: the OTel collector is a single
	// leader-pinned task and hostmetrics can only read its own host, so the only
	// process that can answer "what is eating this machine" is the daemon living
	// on it. Started BEFORE the leadership wait (a standby manager and a worker
	// both block there indefinitely) and before Docker is required (a nil client
	// only costs the docker_up flag). It needs no store: /proc plus the OTLP
	// endpoint is the whole dependency.
	if endpoint != "" {
		startNodeStatSampler(cmd.Context(), dc, log)
	}
	if dockerErr != nil {
		log.Warn().Err(dockerErr).Msg("docker client init failed; /api/cluster/info disabled")
	} else {
		defer func() { _ = dc.Close() }()

		// Every daemon — leader or not — keeps the volume directories of the
		// stateful services the Swarm placed on this node. A standby manager
		// blocks in waitForSwarmLeadership below, so the loop starts here and
		// touches the runtime client only, never the store.
		startVolumeRepairLoop(cmd.Context(), dc, cluster.NewDockerCLIDeployer(os.Stderr), func() string {
			s, _ := storageRoot.Load().(string)
			return s
		}, log)

		// Publish this node's backup plan: the hourly agent archives directories,
		// and only the daemon knows which of them still have a stack behind them.
		//
		// Started BEFORE the leadership wait on purpose. A standby manager can
		// read the service list, and every storage node needs its own plan for the
		// volumes it actually holds — publishing only on the leader left the other
		// managers archiving directories of stacks that no longer exist.
		go publishBackupPlan(cmd.Context(), dc, func(format string, args ...any) {
			log.Warn().Msgf(format, args...)
		})

		// Publish this node's binary version so skew is visible before an
		// upgrade script discovers it.
		//
		// Started BEFORE the leadership wait, for the same reason the backup
		// plan is: a follower manager blocks there indefinitely, so a publisher
		// placed after that wait only ever ran on the leader — measured on a
		// live fleet, where two of three managers never reported a version at
		// all. Workers do start the loop too; Publish asks per tick whether this
		// node can control the swarm and stays silent until it can.
		if host, herr := os.Hostname(); herr == nil {
			pub := &nodever.Publisher{
				Docker: dc, Hostname: host, Version: buildinfo.Version, Log: log,
				Interval: 10 * time.Minute,
			}
			go pub.Loop(cmd.Context())
		}

		// Leader-aware daemon: on a multi-manager cluster the daemon serves
		// ONLY on the current Swarm leader; other managers stand by until
		// Swarm elects them leader (failover). Standalone/local mode returns
		// immediately.
		// Control-plane freshness for EVERY manager at startup, BEFORE the
		// leadership wait, not only on promotion. A follower manager that never
		// leads still needs the current credential and settings rows to answer
		// reads (the observability reader, the console) — restoring only on
		// promotion is why two managers sat on a two-week-old credential table
		// while reporting themselves current.
		//
		// This is the safe window: nothing has the store open yet, so replacing
		// data.db cannot truncate it under a live sqlite connection. Best-effort:
		// a worker, or a swarm without a readable Raft config, must still serve.
		// The mid-run twin of this check is adoptOnLeadershipGain: a node that
		// BECOMES leader later adopts through the open store (close, replace,
		// reopen) on the same predicate.
		if _, err := ensureControlPlaneFresh(cmd.Context(), cfg, dc, log); err != nil {
			log.Warn().Err(err).Msg("control-plane restore at startup had issues (continuing)")
		}

		if err := waitForSwarmLeadership(cmd.Context(), dc, log); err != nil {
			return fmt.Errorf("standby: %w", err)
		}

		// Control-plane freshness: when this node was a standby and just
		// became leader, adopt the newest control-plane snapshot if the
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
		// before any mutation happens. Published through the same Kit the
		// leadership loop uses, so that loop recognises this snapshot as its
		// own and does not adopt it back.
		if err := kit.Snapshot(cmd.Context()); err != nil {
			log.Warn().Err(err).Msg("initial control-plane snapshot had issues")
		}

		checkConfigVersions(cmd.Context(), dc, log)
	}

	// The store is opened only after the leadership + control-plane-restore
	// block: a standby node (non-leader) has no local DB yet, and on promotion
	// the restore must happen BEFORE the DB is opened so the restored control
	// plane is what gets served.
	// Every node's daemon ensures its own storage roots exist: a stateful stack
	// scheduled here binds <volume_root>/<app>/<name>, and the Swarm cannot
	// start a task whose bind source is missing. The path comes from the
	// volume_root setting (default /var/stack/data), so the daemon creates the
	// directory the operator configured — on THIS host, before anything is
	// deployed onto it.
	if err := ensureLocalStorageRoots(cmd.Context(), cfg, log); err != nil {
		log.Warn().Err(err).Msg("ensure local storage roots")
	}

	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — run `pmcluster init` (or `pmcluster join` on a joining node)", cfg.DataDir)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	storageRoot.Store(localVolumeRoot(cmd.Context(), st))

	// The persisted log_level cluster setting overrides the config-file
	// default, so a console change survives daemon restarts. Invalid values
	// are ignored here (a bad setting only surfaces at save time).
	if lvl := st.SettingDefault(cmd.Context(), cluster.SettingLogLevel(), ""); lvl != "" {
		_ = logger.SetLevel(lvl)
	}

	if err := replayRegistryLogins(cmd.Context(), st, cfg, log); err != nil {
		log.Warn().Err(err).Msg("registry re-login had issues; private images may fail to pull")
	}
	// Keep the App's GHCR login fresh: an installation token expires in an hour,
	// and a deploy after that would fail with a 401 no earlier signal announced.
	// 25 minutes means two logins per token lifetime (see refreshGitHubAppLogins).
	go refreshGitHubAppLogins(cmd.Context(), st, cfg, log, 25*time.Minute)

	deployer := cluster.NewDockerCLIDeployer(cmd.OutOrStdout())

	cipher, cipherErr := credentials.Open(cfg.EncryptionKeyPath())
	if cipherErr != nil {
		log.Warn().Err(cipherErr).Msg("encryption key not available; /webhook/* disabled")
	}

	// The in-cluster SeaweedFS store, when enabled (seaweedfs_admin credential
	// present), holds the newest backup copies, so reads — failover restore,
	// `backup restore --from-s3` — prefer it. offen now writes each archive to
	// BOTH this store (via WebDAV) and the external backup_s3_* offsite target
	// (via S3); when the in-cluster store is disabled the offsite target is
	// used directly.
	backupS3 := backupS3Config(cmd.Context(), st, cipher)

	deploySvc := &stacks.Service{Store: st, Deployer: deployer, Docker: dc, Backup: backups.LocalTrigger{Store: st}, Resolver: &stacks.StoreConfigResolver{Store: st, Docker: dc, Cipher: cipher}, VolumeRoot: st.SettingDefault(cmd.Context(), cluster.SettingVolumeRoot(), ""), CertResolver: cluster.CertResolverForMode(st.SettingDefault(cmd.Context(), cluster.SettingTLSMode(), "")), Pins: &stacks.PinResolver{StorageNodes: stacks.ParseStorageNodes(st.SettingDefault(cmd.Context(), cluster.SettingStorageNodes(), "")), StackPin: func(ctx context.Context, stackName string) (string, error) {
		return st.SettingDefault(ctx, stacks.StackPinKey(stackName), ""), nil
	}, SetStackPin: func(ctx context.Context, stackName, node string) error {
		return st.SetSetting(ctx, stacks.StackPinKey(stackName), node)
	}}, Log: log, BackupDir: cluster.BackupRootDir(), S3: backupS3, AppLogLevel: cluster.LoadAppLogLevel(cmd.Context(), st)}

	tlsSvc := certs.NewLocal(st, cipher, dc, deployer,
		cfg.ConfigDir(), buildinfo.Version)

	// The console's restore button goes through this service, so it gets the
	// same drain the CLI has: a restore that writes into a live volume is a
	// corrupt restore, whichever front door it came through.
	backupSvc := &backups.Local{
		Store:         st,
		Run:           backups.LocalTrigger{Store: st}.Trigger,
		ArchiveDir:    cluster.BackupRootDir(),
		RetentionDays: cluster.LoadBackupRetentionDays(cmd.Context(), st),
		S3:            backupS3,
		Offsite:       backupS3FromSettings(cmd.Context(), st),
		Logf: func(format string, args ...any) {
			log.Printf(format, args...)
		},
	}
	if sc, ok := dc.(backups.Scaler); ok {
		backupSvc.Scaler = sc
	}

	deps := server.Deps{
		Lookup:        st,
		Docker:        dc,
		Store:         st,
		DeployService: deploySvc,
		Cipher:        cipher,
		Backups:       backupSvc,
		Settings: func() *settings.Local {
			l := settings.NewLocal(st)
			l.ApplyLogLevel = logger.SetLevel
			l.ApplyStorageNodes = func(v string) error {
				deploySvc.Pins.SetStorageNodes(stacks.ParseStorageNodes(v))
				return nil
			}
			return l
		}(),
		MCP:       &server.MCPOptions{AllowOps: mcpAllowOps()},
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
		// The GitHub App is one encrypted registration in the store; both the
		// console card and the Actions broker share the same cached minter, so
		// ten parallel pulls mint one token.
		deps.GitHubApp = newGitHubAppLocal(st, cipher, log)
		deps.GitHubBroker = &githubapp.Broker{Secrets: deps.GitHubApp, Minter: deps.GitHubApp, Log: &log}
		// Observability reads the backend through the published loopback
		// endpoint with the managed credential. A missing credential leaves the
		// reader with Swarm inventory only, so the console still reports node
		// health and every telemetry read answers "unavailable".
		obsReader, obsErr := newLocalObservabilityReader(cmd.Context(), st, cipher, dc)
		if obsErr != nil {
			log.Warn().Err(obsErr).Msg("observability telemetry source unavailable — inventory only")
		}
		// The daemon read path is memoised for 10s so a burst of identical
		// reads (console fragments, an agent polling through MCP) pays Parseable
		// once. Live/incremental reads bypass the cache by construction.
		deps.Observability = observability.NewCachedReader(obsReader)
	}
	handler := server.New(deps)

	// The gate answers 503 while a leadership-gain adoption is swapping the
	// store, so no request is answered from the revision being replaced.
	gate := &adoptionGate{}
	handler = gate.wrap(handler)

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Leader-only control loop: on becoming Swarm leader the daemon adopts the
	// newest control-plane kit (when this node is behind it), runs the reconcile
	// loop (platform configs + app-stack drift + health snapshot) and the
	// control-plane Raft snapshot loop; on losing leadership the loops stop.
	// Non-manager/standalone nodes serve without the loops.
	leadership := WatchSwarmLeadership(ctx, dc, log)
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
		// Source marks applies made by this loop. Its output is discarded, so
		// without the marker an operator's later `cluster update` reports
		// "nothing to redeploy" with no way to learn the loop already applied it.
		UpdateInput: cluster.UpdateInput{ConfigDir: cfg.ConfigDir(), Version: buildinfo.Version, Source: "reconcile"},
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
				// Name this node as the leader BEFORE the reconcile pass: the
				// collector's placement CONSTRAINT reads this label, so a
				// missing label means an unschedulable collector, not a
				// slightly misplaced one. This also covers a daemon that starts
				// while already holding leadership — the watch emits its
				// initial state, so this runs on that first pass too.
				if syncLeaderLabel(ctx, dc, true, log) {
					replaceNodeStatCollector(ctx, dc, log)
				}
				// This node just gained leadership. Its data.db may be older
				// than the newest Raft-replicated kit (the previous leader
				// snapshotted credentials/settings this node never adopted), so
				// adopt BEFORE the reconcile loop reads settings, before this
				// node answers control-plane reads, and before publishing:
				// adopting after a publish would overwrite the deployed state
				// with a stale snapshot.
				//
				// Exactly once per transition: the leadership channel emits only
				// on a state change, so a node that stays leader never re-runs
				// this — a per-tick check would close and reopen the store under
				// every request, for nothing.
				adoptOnLeadershipGain(ctx, kit, st, cipher, cfg, gate, log)

				if err := kit.Snapshot(ctx); err != nil {
					log.Warn().Err(err).Msg("control-plane snapshot on leadership gain had issues")
				}
				var loopCtx context.Context
				loopCtx, loopCancel = context.WithCancel(ctx)
				go reconciler.Loop(loopCtx)
				go kit.Loop(loopCtx, controlPlaneSnapshotInterval())
				// The collector writes its node-stats JSONL on the leader's
				// disk; only this daemon can turn it into rows for the console.
				go runNodeStatTailer(loopCtx, st, dc, log)
				// Keep the label true for as long as this node is leader. One
				// write per election is not enough now that placement is a
				// constraint: a write that failed during the election leaves the
				// collector with no matching node at all.
				go keepLeaderLabel(loopCtx, dc, log)
			} else {
				log.Info().Msg("control loop: stopped (not the swarm leader)")
				syncLeaderLabel(ctx, dc, false, log)
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
	raw := st.SettingDefault(ctx, cluster.SettingReconcileInterval(), "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// ensureLocalStorageRoots creates the storage directories this node owns: the
// configured volume root and the backup directory. A stateful stack scheduled
// onto this node binds <volume_root>/<app>/<name>, and the Swarm cannot start a
// task whose bind source is missing, so each node's daemon prepares its own
// filesystem. The path is the volume_root setting — the directory the operator
// selected — falling back to manifest.DefaultVolumeRoot when unset.
func ensureLocalStorageRoots(ctx context.Context, cfg *config.Config, log zerolog.Logger) error {
	root := manifest.DefaultVolumeRoot
	// Read the setting only when the store already exists — opening it would
	// otherwise create an empty data.db and defeat the "run pmcluster init"
	// guard below.
	if _, statErr := os.Stat(cfg.DBPath()); statErr == nil {
		if st, err := store.Open(cfg.DBPath()); err == nil {
			root = localVolumeRoot(ctx, st)
			_ = st.Close()
		}
	}
	seen := map[string]bool{}
	for _, dir := range []string{root, cluster.BackupRootDir()} {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	log.Info().Str("volume_root", root).Msg("local storage roots ensured")
	return nil
}

// mcpAllowOps decides which writes the MCP endpoint permits, from
// PMCLUSTER_MCP_WRITES:
//
//	unset / "service-config" (default) — update a service-scoped config's content
//	"none"                             — reads only
//	"all"                              — every non-dangerous write
//
// Dangerous operations (exec, secret reveal, deletions, TLS, backup restore) are
// never enabled from here: they stay manual by construction.
func mcpAllowOps() map[string]bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PMCLUSTER_MCP_WRITES"))) {
	case "none", "read", "readonly":
		return map[string]bool{}
	case "all", "write":
		all, err := mcpserver.AllWrites(docs.OpenAPI)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp: cannot derive the write allowlist (%v); serving reads only\n", err)
			return map[string]bool{}
		}
		return all
	default:
		return mcpserver.DefaultAllowedOps
	}
}
