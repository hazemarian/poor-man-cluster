package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backup"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/deploy"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/logger"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/server"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/telemetry"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the pmcluster HTTP daemon (REST API + webhook receiver)",
	Long: `Starts the long-running pmcluster daemon. Listens on 127.0.0.1:9090 by
default; Traefik (running in the swarm) routes pmcluster.<domain> to it via
host.docker.internal:9090.

The data directory ($HOME/.pmcluster by default) must already be initialised
via 'pmcluster init'.`,
	RunE: runServe,
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — run `pmcluster init` first", cfg.DataDir)
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

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	dc, dockerErr := docker.New()
	if dockerErr != nil {
		log.Warn().Err(dockerErr).Msg("docker client init failed; /api/cluster/info disabled")
	} else {
		defer func() { _ = dc.Close() }()

		checkConfigVersions(cmd.Context(), dc, log)
	}

	if err := replayRegistryLogins(cmd.Context(), st, cfg, log); err != nil {
		log.Warn().Err(err).Msg("registry re-login had issues; private images may fail to pull")
	}

	deployer := cluster.NewDockerCLIDeployer(cmd.OutOrStdout())
	deploySvc := &deploy.Service{Store: st, Deployer: deployer, Docker: dc, Backup: backup.LocalTrigger{Store: st}, Resolver: &deploy.StoreConfigResolver{Store: st}}

	cipher, cipherErr := credentials.Open(cfg.EncryptionKeyPath())
	if cipherErr != nil {
		log.Warn().Err(cipherErr).Msg("encryption key not available; /webhook/* disabled")
	}

	hostCerts := &server.HostCertService{
		Store: st,
		Apply: func(ctx context.Context, host, certPEM, keyPEM string) (*store.SiteCertRow, error) {
			if cipher == nil {
				return nil, fmt.Errorf("encryption key unavailable; cannot refresh Traefik")
			}
			return cluster.ApplyCert(ctx, cluster.SiteCertDeps{
				Store:       st,
				Cipher:      cipher,
				Docker:      dc,
				Deployer:    deployer,
				Provisioner: ooProvisioner(st, cipher, io.Discard, cluster.PersistedDomain(ctx, st)),
			}, cfg.ConfigDir(), buildinfo.Version, host, certPEM, keyPEM, true)
		},
		Remove: func(ctx context.Context, host string) error {
			if cipher == nil {
				return fmt.Errorf("encryption key unavailable; cannot refresh Traefik")
			}
			return cluster.RemoveCert(ctx, cluster.SiteCertDeps{
				Store:       st,
				Cipher:      cipher,
				Docker:      dc,
				Deployer:    deployer,
				Provisioner: ooProvisioner(st, cipher, io.Discard, cluster.PersistedDomain(ctx, st)),
			}, cfg.ConfigDir(), buildinfo.Version, host, true)
		},
	}

	handler := server.New(server.Deps{
		Lookup:        st,
		Docker:        dc,
		Store:         st,
		DeployService: deploySvc,
		Cipher:        cipher,
		BackupTrigger: backup.LocalTrigger{Store: st},
		HostCerts:     hostCerts,
		SiteCert: &server.SiteCertService{
			Store: st,
			Apply: func(ctx context.Context, domain, certPEM, keyPEM string) (*store.SiteCertRow, error) {
				if cipher == nil {
					return nil, fmt.Errorf("encryption key unavailable; cannot refresh Traefik")
				}
				return cluster.ApplyCert(ctx, cluster.SiteCertDeps{
					Store:       st,
					Cipher:      cipher,
					Docker:      dc,
					Deployer:    deployer,
					Provisioner: ooProvisioner(st, cipher, io.Discard, domain),
				}, cfg.ConfigDir(), buildinfo.Version, domain, certPEM, keyPEM, true)
			},
		},
		Update: &server.UpdateService{
			Update: func(ctx context.Context) (*cluster.UpdateResult, error) {
				if cipher == nil {
					return nil, fmt.Errorf("encryption key unavailable; cannot run cluster update")
				}
				return cluster.Update(ctx, cluster.UpdateDeps{
					Store:       st,
					Cipher:      cipher,
					Docker:      dc,
					Deployer:    deployer,
					Provisioner: ooProvisioner(st, cipher, io.Discard, cluster.PersistedDomain(ctx, st)),
					Stdout:      io.Discard,
				}, cluster.UpdateInput{ConfigDir: cfg.ConfigDir(), Version: buildinfo.Version})
			},
		},
	})

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
func checkConfigVersions(ctx context.Context, dc docker.Client, log zerolog.Logger) {
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
