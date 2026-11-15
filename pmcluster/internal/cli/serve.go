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

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodeunits"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/docs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/apikeys"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/certs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ci"
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
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/noderuntime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodever"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/reconcile"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/secrets"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/server"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/settings"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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

	// storageRoot is published once the local store is open (leader path).
	// Before that the volume repair still handles stack volumes, which carry
	// their host directory on the volume itself and need no root.
	var storageRoot atomic.Value
	storageRoot.Store("")

	// Per-node loop handles, started inside the Docker branch below and used
	// again after the leadership wait (a promoted worker must stop its loopback
	// API before the leader API binds the same port).
	var (
		localAPIStop func()
		localAPIDone <-chan struct{}
		// mirrorRan records that THIS process ran the snapshot mirror before it became
		// leader. It separates "never a follower" (nothing to vouch for) from "a follower
		// whose polls never landed" (a refusal) at the leadership-gain check.
		mirrorRan bool
	)

	dc, dockerErr := docker.New()
	// The store is resolved LATE: a standby must not open a stale database, and on
	// promotion the restored control plane has to be what gets opened. The
	// per-node loops below take this handle, so they can start before the
	// leadership wait without pre-empting that ordering.
	served := &noderuntime.LateStore{}
	hostname, _ := os.Hostname()

	// Self-telemetry is SUPERVISED, not decided once at startup. The collector
	// can be rescheduled long after this daemon boots, and a node that gave up
	// while it was away used to stay dark until someone restarted it — measured
	// on a live worker whose exporter was disabled at boot and never
	// reconsidered, leaving a five-node cluster showing one row in the console.
	// Started here (docker client in hand) and still well before the leadership
	// wait, so a standby manager and a worker both keep reporting.
	// The sampler starts HERE, not from the telemetry supervisor's first-endpoint hook:
	// gating it on an exporter is how four nodes vanished from the panel when the exporter
	// was leader-only, and its report half (node → leader over mTLS) needs no collector at
	// all. Its OTLP half is a no-op until the probe finds one.
	go func() {
		if dc != nil {
			noderuntime.StartNodeStatSampler(cmd.Context(), dc, log)
		}
	}()
	telemetryCtl := noderuntime.NewTelemetrySupervisor(log, hostname, func() {
	})
	go telemetryCtl.Loop(cmd.Context(), cfg.OTLPEndpoint, func(ctx context.Context) bool {
		// dc is nil when the client could not be built (a malformed DOCKER_HOST is
		// enough) — the daemon deliberately keeps serving without it: /api/cluster/info
		// is disabled and everything else carries on. This closure ran anyway and
		// dereferenced the nil interface, so a daemon that had degraded correctly
		// PANICKED on the supervisor's first probe. The two neighbouring closures
		// already guard; this one is the one that was missed.
		if dc == nil {
			return false
		}
		leader, _, err := noderuntime.IsSwarmLeader(ctx, dc, hostname)
		return err == nil && leader
	}, func(ctx context.Context) string {
		// A follower answers where the leader is, and it finds it the way the mTLS
		// channel does: every manager address, the first that answers on the collector
		// port wins. Re-probed every minute, so a leadership change re-points the
		// exporter without a restart.
		if dc == nil {
			return ""
		}
		return collectorEndpointFor(ctx, func(c context.Context) ([]string, error) {
			return noderuntime.ManagerHosts(c, dc)
		}, dialCollector)
	})
	defer telemetryCtl.Stop(context.Background())
	// worker decides which runner path this node takes: a swarm WORKER cannot read
	// cluster settings (Docker configs are manager-only), so it learns its runner
	// decision from the leader instead. Cheap and best-effort: any doubt (no
	// docker, no swarm) keeps the settings-based path.
	worker := dockerErr == nil && noderuntime.IsSwarmWorker(cmd.Context(), dc)
	// standby says this node is not the leader right now (a worker, or a follower
	// manager), so its API must answer on loopback only until it is elected. A
	// standby that bound all interfaces would be a second, stale control plane.
	standby := false
	if dockerErr == nil {
		if worker {
			standby = true
		} else {
			leader, found, err := noderuntime.IsSwarmLeader(cmd.Context(), dc, hostname)
			standby = noderuntime.IsStandby(worker, leader, found, err)
		}
	}

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
	if dockerErr != nil {
		log.Warn().Err(dockerErr).Msg("docker client init failed; /api/cluster/info disabled")
	} else {
		defer func() { _ = dc.Close() }()

		// Every daemon — leader or not — keeps the volume directories of the
		// stateful services the Swarm placed on this node. A standby manager
		// blocks in noderuntime.WaitForSwarmLeadership below, so the loop starts here and
		// touches the runtime client only, never the store.
		noderuntime.StartVolumeRepairLoop(cmd.Context(), dc, cluster.NewDockerCLIDeployer(os.Stderr), func() string {
			if s, _ := storageRoot.Load().(string); s != "" {
				return s
			}
			// storageRoot is only published once the local store is open, which
			// only the leader does — so on a follower this used to be "". An
			// empty root makes every bind path fall outside the managed layout,
			// which silently turned off the bind half of the repair exactly on
			// the nodes that do not have a store to read the setting from.
			if v := os.Getenv("PMCLUSTER_VOLUME_ROOT"); v != "" {
				return v
			}
			return manifest.DefaultVolumeRoot
		}, log)

		// Publish this node's backup plan: the hourly agent archives directories,
		// and only the daemon knows which of them still have a stack behind them.
		//
		// Started BEFORE the leadership wait on purpose. A standby manager can
		// read the service list, and every storage node needs its own plan for the
		// volumes it actually holds — publishing only on the leader left the other
		// managers archiving directories of stacks that no longer exist.
		go noderuntime.PublishBackupPlan(cmd.Context(), dc, func(format string, args ...any) {
			log.Warn().Msgf(format, args...)
		})

		// Converge this node's systemd protection with the binary that is now
		// running: an upgrade swaps the binary and restarts the service, so a
		// node that never runs `cluster up` otherwise keeps the unit it was
		// installed with — measured after the watchdog shipped, where the leader
		// had memory bounds and an active watchdog timer and the OOM-killed
		// worker had neither.
		// The unit always carries --docker-socket: that is the CAPABILITY. Whether it
		// acts is decided at run time from the node policy document, because a worker
		// has no store to read the setting from — the first version gated the unit on
		// the local store, which on a worker is always absent, so the feature silently
		// did nothing on exactly the nodes it was written for (measured on
		// nxt-sw-6-w: setting true, unit never gained the flag).
		sock := noderuntime.DefaultDockerSocket
		// Only a store that HOLDS the decision may write the node policy. A worker's
		// store cannot answer a leader-owned setting, and writing its default there
		// disarmed the docker watchdog at the worst possible moment (nxt-sw-6-w,
		// 2026-11-13: dockerd dead, the daemon restarted, the policy was rewritten as
		// false, and the watchdog then sat through three failed probes doing nothing).
		// On a worker the assignment carries the leader's answer — runners.Agent
		// writes it, and that write is allowed to overwrite this file.
		// Written once here AND then kept current by nodePolicyLoop below: this
		// one call happens at startup, and the node that OWNS the setting has no
		// runner agent to push a new assignment at it. Measured on the leader
		// (2026-11-14): `settings set node_docker_restart=true` reached a follower
		// within 50s through its agent while the leader's own policy still said
		// false, because its only write had been at boot — the armed state landed
		// everywhere except the node that decided it.
		applyNodePolicyOnce(cmd.Context(), cfg, log)
		go nodePolicyLoop(cmd.Context(), cfg, log)
		go nodeunits.ConvergeSystemd(func(f string, a ...any) { log.Info().Msgf(f, a...) }, sock, cfg.DataDir)

		// The console's storage toggle sets INTENT; provisioning is local work (loop
		// file, LVM, mkfs, mounts), so the node that the intent names has to do it —
		// and its daemon is root there. Asynchronous because it takes minutes and must
		// never delay serving; best effort because a node that cannot provision must
		// still run.
		go func() {
			st, err := openExistingStore(cfg.DBPath())
			if err != nil {
				return // no store on this node: not a manager, cannot hold storage either
			}
			defer func() { _ = st.Close() }()
			host, herr := os.Hostname()
			if herr != nil {
				return
			}
			logf := func(f string, a ...any) { log.Info().Msgf(f, a...) }
			// Once at start, then periodically. Start-only was wrong for the console:
			// an operator flips the toggle and expects the node to act, but the node's
			// intent was read before the click and nothing would happen until the next
			// restart — the toggle would look broken for hours. The check is cheap
			// (findmnt + one readdir) and only provisions when the answer changed.
			convergeStorage(cmd.Context(), st, host, logf)
			tick := time.NewTicker(2 * time.Minute)
			defer tick.Stop()
			for {
				select {
				case <-cmd.Context().Done():
					return
				case <-tick.C:
					convergeStorage(cmd.Context(), st, host, logf)
				}
			}
		}()

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

		// The per-node runner machinery and the filtered docker socket start HERE,
		// before the leadership wait — the same pattern as the backup plan and the
		// version publisher above, and for the same measured reason: a WORKER
		// blocks in that wait forever, so every loop placed after it only ever ran
		// on the leader. That is exactly why the runner feature of v0.2.267 was
		// inert: the only nodes allowed to host a runner are the ones that never
		// got past the wait.
		//
		//   - a WORKER runs runners.Agent against the leader's assignment (its
		//     settings are unreadable here, see internal/runners/assignment.go);
		//   - a manager keeps the store-based reconcile, which starts as soon as
		//     the store and the App exist (further down) — it cannot start before
		//     them, and a standby manager is refused as a runner host anyway.
		noderuntime.StartPerNodeLoops(cmd.Context(), noderuntime.PerNodeDeps{
			Worker:     worker,
			Hostname:   hostname,
			Container:  &runners.CLIDocker{},
			Assignment: noderuntime.AssignmentClientFor(dc),
			Client:     dc,
			StatePath:  noderuntime.AgentStatePath(cfg.DataDir),
			Log:        log,
		})

		// A STANDBY node (a worker, or a follower manager) serves its API on
		// LOOPBACK only. It answers nothing cluster-wide — but the runner loop
		// needs a local surface, and doctor / `/health` must work on every node
		// instead of failing because the daemon never bound at all. The leader
		// keeps binding what the operator configured (all interfaces), below.
		if standby {
			localAPIStop, localAPIDone = noderuntime.StartWorkerAPI(cmd.Context(), dc, noderuntime.APIAddr(cfg.ListenAddr, true), log)
			defer func() {
				if localAPIStop != nil {
					localAPIStop()
				}
			}()
		}

		// Leader-aware daemon: on a multi-manager cluster the daemon serves
		// ONLY on the current Swarm leader; other managers stand by until
		// Swarm elects them leader (failover). Standalone/local mode returns
		// immediately.
		// What this node's cluster state is, stated at boot, before anything opens the
		// store. The old path restored data.db from a Raft config here; the store is
		// seeded by the mirror now, so there is nothing to restore and nothing to copy —
		// only a state to report. A manager with no data yet starts, serves /health and
		// says it is waiting for the first snapshot.
		noderuntime.ConfirmMirrorAtStartup(cmd.Context(), cfg, log)

		// The follower's mirror (docs/adr/0003-cluster-state-by-pull.md): a manager that is
		// NOT the leader pulls the leader's snapshot into its own store. Two ordering rules,
		// both load-bearing:
		//
		//   * it starts BEFORE the leadership wait, because a follower manager blocks there
		//     forever (the runner assignment loop has the same shape for the same reason);
		//   * it is joined (not merely cancelled) before this node serves as the writer, so
		//     there is one writer to a row at a time.
		//
		// A WORKER does not mirror: it has no store, and opening one would seed an empty
		// database whose answers look plausible.
		var mirrorStop func()
		var mirrorDone <-chan struct{}
		if standby && !worker {
			mirrorStop, mirrorDone = noderuntime.StartSnapshotMirror(cmd.Context(), noderuntime.SnapshotMirrorFor(cfg, dc, hostname, log))
			mirrorRan = true
			defer func() {
				if mirrorStop != nil {
					mirrorStop()
					<-mirrorDone
				}
			}()
		}

		if err := noderuntime.WaitForSwarmLeadership(cmd.Context(), dc, log); err != nil {
			return fmt.Errorf("standby: %w", err)
		}

		// This node is the leader now, so the mirror has to be gone — finished its last
		// tick, handle closed — BEFORE the leader's own store is opened below. Two
		// writers to one row is a race whose winner nobody can predict.
		if mirrorStop != nil {
			mirrorStop()
			<-mirrorDone
			mirrorStop = nil
		}

		// This node now leads (or Docker went away and it serves standalone). If
		// it was a WORKER, its loopback API has to come down before the leader API
		// binds the same port; waiting on the channel means the listener is really
		// closed, not merely asked to close.
		if localAPIStop != nil {
			localAPIStop()
			<-localAPIDone
			localAPIStop = nil
		}

		// Nothing is restored or adopted here: this node serves the store it has been
		// mirroring, and the leadership watcher below decides — with the mirror's age in
		// hand — whether it may vouch for it before it becomes the writer.

		// First publication as leader: the KEY config only. The data kit is retired —
		// the mirror is the document, so a promoted manager has nothing to fetch from a
		// Docker config (ADR 0005).
		if err := kit.Snapshot(cmd.Context()); err != nil {
			log.Warn().Err(err).Msg("initial control-plane key snapshot had issues")
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

	// The store is seeded by the MIRROR, so a manager that holds no data yet still has
	// to start: it answers /health, serves empty tables with the mirror's state attached,
	// and says plainly that it is waiting for the first snapshot. Refusing to start (the
	// old behaviour) turned a node whose data directory was lost, or one that has not
	// polled yet, into a crash loop — and a crash loop is not an explanation.
	st, err := openServeStore(cfg, log)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	// Now the per-node loops can see the settings they were started without.
	served.Set(st)
	storageRoot.Store(noderuntime.LocalVolumeRoot(cmd.Context(), st))

	// The persisted log_level cluster setting overrides the config-file
	// default, so a console change survives daemon restarts. Invalid values
	// are ignored here (a bad setting only surfaces at save time).
	if lvl := st.SettingDefault(cmd.Context(), cluster.SettingLogLevel(), ""); lvl != "" {
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
	},
		// A pin outlives the node it names: the constraint stays spelled
		// correctly, the rendered hash stops changing, and the service waits
		// forever on a host that left the swarm. The resolver asks what is live so
		// it can move a platform-made pin off a node that is gone.
		LiveNodes: func(ctx context.Context) (map[string]bool, error) {
			nodes, err := dc.NodeList(ctx)
			if err != nil {
				return nil, err
			}
			live := make(map[string]bool, len(nodes))
			for _, n := range nodes {
				live[n.Hostname] = true
			}
			return live, nil
		},
		// The platform's own stacks hold volumes deliberately outside the storage
		// set (the object store lives on a worker); customer stacks may not.
		Exempt: platformStackSet(),
		OnRelocate: func(ctx context.Context, stackName, from, to string) {
			log.Warn().Str("stack", stackName).Str("from", from).Str("to", to).
				Msg("placement — pinned to a node that is not in the swarm; moved to a live storage node (the data did not follow: restore it from the stack's archive)")
		},
	}, Log: log, BackupDir: cluster.BackupRootDir(), S3: backupS3, AppLogLevel: cluster.LoadAppLogLevel(cmd.Context(), st),
		// Pulls authenticate with the stored read-only PAT, applied once per
		// deploy — before `--with-registry-auth` freezes it into each spec.
		// The App covers clone/build/broker; it is not a pull credential.
		BeforeApply: func(ctx context.Context) { loginBeforeDeploy(ctx, st, cfg, log) },
	}

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
	// The runner manager is built before the HTTP handler: the console's
	// Runners panel calls this same manager through /api/github/runners, and a
	// second instance would keep a second copy of the kill holds.
	// One sink, two users: the assignment listener writes what nodes report, the
	// manager reads it for the console. The console is served by the LEADER and the
	// volumes belong to the node that runs the runner, so without this the page could
	// only ever warn about a runner living on the leader itself.
	cacheReports := runners.NewCacheReports()
	runnerMgr := &runners.Manager{
		Store:  st,
		Docker: &runners.CLIDocker{},
		Nodes:  dc,
		Node:   hostname,
		// The console row and the reconcile use the same size rule the
		// assignment resolver applies, so a refused node's row can say why.
		NodeMemory: func(ctx context.Context, node string) (int64, bool) {
			return runners.NodeMemoryMiB(ctx, st, node, time.Now())
		},
		Log: log.With().Str("component", "runners").Logger(),
	}
	if deps.GitHubApp != nil {
		// The manager registers and de-registers runners with the same cached
		// installation token the broker and the console use; no PAT and no
		// second credential exist for this feature.
		runnerMgr.GitHub = deps.GitHubApp
	}
	runnerMgr.CacheReports = cacheReports
	deps.GitHubRunners = runnerMgr

	// The CI board reads Actions with the SAME installation token the runner
	// feature uses — no second credential, no webhook, no poller. It is built here
	// so an operator can answer "what is my CI doing" from the console instead of
	// opening ten repository pages; when there is no App there is no board, and the
	// panel says so rather than showing an empty table that reads as "all green".
	if deps.GitHubApp != nil && deps.GitHubApp.Client != nil {
		deps.CIBoard = &ci.Board{Src: &ci.Live{App: deps.GitHubApp, Client: deps.GitHubApp.Client}}
	}

	// Runners are a PER-NODE feature and the settings-based half starts here, the
	// moment its inputs exist (store, cipher, App). It used to live BELOW the
	// leadership watcher, which is why it only ever ran on the leader; a leader is
	// refused as a runner host, so it was a loop that could never do anything. A
	// WORKER does not run this at all: it has no readable settings and applies the
	// leader's assignment instead (started before the leadership wait, above).
	if !worker {
		go runRunnerLoop(cmd.Context(), runnerMgr, st, log, nil)
	}

	// The runner ASSIGNMENT endpoint: the one thing this daemon serves to other
	// nodes, on its own mTLS listener, and only while it leads.
	//
	// SCOPE: this middleware is mounted on THIS route and on nothing else. Not on
	// /api/*, not on /metrics, not on the console. A compromised worker must not
	// gain a second readable endpoint, and a later change that mounts swarmtls
	// anywhere else is a deliberate widening of the cluster's attack surface —
	// review it as one.
	handler := server.New(deps)

	// The gate answers 503 while a leadership-gain adoption is swapping the
	// store, so no request is answered from the revision being replaced.
	gate := &noderuntime.AdoptionGate{}
	handler = gate.Wrap(handler)

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The runner ASSIGNMENT endpoint: the one thing this daemon serves to other
	// nodes, on its own mTLS listener bound to this node's swarm data-path
	// address. It starts only here — after the leadership wait returned, so this
	// node is the leader, and after the store opened, because the answer is
	// resolved from cluster settings.
	//
	// SCOPE: swarmtls is mounted on THIS route and on nothing else. Not on
	// /api/*, not on /metrics, not on the console. A compromised worker must not
	// gain a second readable endpoint, and a later change that mounts it anywhere
	// else is a deliberate widening of the cluster's attack surface — review it
	// as one.
	if !worker {
		noderuntime.StartAssignmentListener(ctx, noderuntime.AssignmentListenerOptions{
			Store:        st,
			Hostname:     hostname,
			CacheReports: cacheReports,
			Docker:       dc,
			GitHub:       runnerMgr.GitHub,
			Deregister:   runnerMgr.DeregisterName,
			Held: func(name string) bool {
				return runnerMgr.HeldName(ctx, name)
			},
			Log: log,
		})
	}

	// Leader-only control loop: on becoming Swarm leader the daemon adopts the
	// newest control-plane kit (when this node is behind it), runs the reconcile
	// loop (platform configs + app-stack drift + health snapshot) and the
	// control-plane Raft snapshot loop; on losing leadership the loops stop.
	// Non-manager/standalone nodes serve without the loops.
	leadership := noderuntime.WatchSwarmLeadership(ctx, dc, log)
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
				// Nothing to label: the collector follows the leader by NAME at
				// render time, and nothing else reads pmcluster.leader.
				// This node just gained leadership. It is ALREADY holding the cluster's
				// state (it has been mirroring it), so there is nothing to adopt: the check
				// is whether there is anything to serve. A stale mirror is SERVED (the
				// operator's decision) and its age is logged; a node that has never mirrored
				// and holds nothing refuses, loudly, and starts no loop.
				//
				// Exactly once per transition: the leadership channel emits only on a
				// state change, so a node that stays leader never re-runs this.
				if !noderuntime.AdoptOnLeadershipGain(ctx, kit, st, gate, mirrorRan, log) {
					log.Error().Msg("control loop NOT started: this node has no cluster state to serve; reads keep answering with the mirror's age, and restarting pmcluster makes it re-enter standby and mirror again")
					continue
				}

				if err := kit.Snapshot(ctx); err != nil {
					log.Warn().Err(err).Msg("control-plane key snapshot on leadership gain had issues")
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

	// The leader binds what the operator configured (all interfaces by default:
	// the edge container reaches it through host.docker.internal, and Traefik
	// routes pmcluster.<domain> here). noderuntime.APIAddr states the rule in one place and
	// leaves the value untouched for a non-worker.
	listenAddr := noderuntime.APIAddr(cfg.ListenAddr, false)
	log.Info().Str("addr", listenAddr).Msg("pmcluster serve listening")
	if err := server.Run(ctx, listenAddr, handler); err != nil {
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
		if st, err := openExistingStore(cfg.DBPath()); err == nil {
			root = noderuntime.LocalVolumeRoot(ctx, st)
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

// runRunnerLoop is the per-node runner reconcile for a node whose decision comes
// from cluster SETTINGS (a manager or a standalone box). It runs on every such
// daemon, not only the leader: the runner is a local container, so only the node
// itself can create, restart or remove it. A node that is not in
// github_runner_nodes does the teardown half and nothing else.
//
// onPass, when set, is called after every pass (a test hook; production passes
// nil).
func runRunnerLoop(ctx context.Context, mgr *runners.Manager, st *store.Store, log zerolog.Logger, onPass func()) {
	every := time.Duration(reconcileIntervalSeconds(ctx, st)) * time.Second
	if every <= 0 {
		every = time.Minute
	}
	tick := func() {
		if onPass != nil {
			onPass()
		}
		res, err := mgr.Reconcile(ctx)
		if len(res.Registered) > 0 || len(res.Removed) > 0 {
			log.Info().Strs("registered", res.Registered).Strs("removed", res.Removed).
				Msg("runners — this node's runner reconciled")
		}
		for _, w := range res.Warnings {
			log.Warn().Str("warning", w).Msg("runners — warning")
		}
		if err != nil {
			// A refusal is the expected outcome of an enabled-but-incomplete
			// configuration; the settings surface refuses it first, so reaching
			// here means the stored settings drifted.
			log.Warn().Err(err).Msg("runners — this node's runner pass refused")
			if st != nil {
				if aerr := cluster.AppendApplyWarnings(ctx, st, "runners", "", []string{err.Error()}); aerr != nil {
					log.Debug().Err(aerr).Msg("runners — could not record the refusal")
				}
			}
			return
		}
		if st != nil && len(res.Warnings) > 0 {
			if aerr := cluster.AppendApplyWarnings(ctx, st, "runners", "", res.Warnings); aerr != nil {
				log.Debug().Err(aerr).Msg("runners — could not record the warning")
			}
		}
	}
	tick()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// nodePolicyInterval is how often a node re-reads the watchdog decision. A
// minute matches the watchdog timer: a setting turned on is armed before the
// second probe that could act on it.
const nodePolicyInterval = time.Minute

// applyNodePolicyOnce writes this node's watchdog policy from its store when the
// store holds the decision, and reports whether the file changed. A store that
// cannot answer (a worker's, which has no leader-owned settings, or a follower
// that has not mirrored the row) is left alone: writing the default there is
// what once disarmed a node's docker watchdog during the very outage it existed
// for.
func applyNodePolicyOnce(ctx context.Context, cfg *config.Config, log zerolog.Logger) bool {
	if _, statErr := os.Stat(cfg.DBPath()); statErr != nil {
		return false
	}
	st, err := openExistingStore(cfg.DBPath())
	if err != nil {
		return false
	}
	defer func() { _ = st.Close() }()
	armed, known := cluster.LoadNodeDockerRestartKnown(ctx, st)
	if !known {
		return false
	}
	want := runners.NodePolicy{DockerRestart: armed}
	if current := runners.ReadNodePolicy(cfg.DataDir); current == want {
		return false
	}
	if werr := runners.WriteNodePolicy(cfg.DataDir, want); werr != nil {
		log.Warn().Err(werr).Msg("could not write the node policy; the watchdog keeps its previous setting")
		return false
	}
	log.Info().Bool("docker_restart", armed).
		Msg("node policy applied — the watchdog's docker restart follows the cluster setting")
	return true
}

// nodePolicyLoop keeps the policy current for the life of the daemon, so a
// setting change reaches a running node without waiting for its next restart.
func nodePolicyLoop(ctx context.Context, cfg *config.Config, log zerolog.Logger) {
	tick := time.NewTicker(nodePolicyInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			applyNodePolicyOnce(ctx, cfg, log)
		}
	}
}

// platformStackSet is the set of stacks the platform deploys itself: their volumes
// are infrastructure, not customer data, so the storage-node rule does not apply.
func platformStackSet() map[string]bool {
	return map[string]bool{
		string(cluster.StackInfra): true, string(cluster.StackEdge): true,
		string(cluster.StackObservability): true, string(cluster.StackBackup): true,
		string(cluster.StackSSO): true,
	}
}
