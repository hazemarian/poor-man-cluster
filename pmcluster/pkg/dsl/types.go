// Package dsl is the public schema for pmcluster's application manifest.
// Customers write the small form here; the translator (internal/manifest)
// expands it to a verbose Compose YAML, auto-injecting traefik-net /
// monitoring-net membership, Traefik labels, standard service labels, the
// networks block, external secret declarations, and default
// restart/update policies.
//
// JSON tags throughout: sigs.k8s.io/yaml decodes YAML via JSON, giving us
// strict unknown-field rejection.
package dsl

// App is the top-level manifest. Exactly one per file.
type App struct {
	Name    string `json:"app"`
	Env     string `json:"env"`
	Domain  string `json:"domain"`
	Version string `json:"version"`

	Registry string `json:"registry,omitempty"`
	RepoURL  string `json:"repo_url,omitempty"`
	EnvFile  string `json:"env_file,omitempty"`

	// Platform marks this stack as platform-managed (infra, edge,
	// observability, backup, sso). Platform stacks carry the
	// io.pmcluster.platform=true label on every service, are excluded from
	// the app-stack drift loop, are NOT user-deletable via the API, and are
	// only removed by `cluster down --purge`. User-facing app manifests must
	// leave this false.
	Platform bool `json:"platform,omitempty"`

	// Networks lists additional EXTERNAL swarm networks every service in
	// this stack joins, beyond the auto-injected private overlay (and the
	// traefik-net/monitoring-net that exposure implies). Platform stacks use
	// it to join traefik-net / monitoring-net explicitly (e.g. the OTel
	// collector joins monitoring-net without exposing anything).
	Networks []string `json:"networks,omitempty"`

	// Secrets are listed at App level so the translator can emit the
	// matching top-level block (secrets: external: true, etc.). Volumes are
	// declared per-service only — the writer auto-declares named volumes and
	// relocates every volume under the volume_root (/var/stack/data by default).
	Secrets []string `json:"secrets,omitempty"`

	// Volumes declares top-level NAMED volumes verbatim (name → optional
	// driver declaration; empty value = plain local volume). Volumes listed
	// here are NOT relocated under the volume root — platform stacks keep
	// their stateful volumes exactly where they are today (openobserve_data,
	// traefik_acme, pmui-data) so a pipeline unification never strands or
	// silently migrates existing data.
	Volumes map[string]string `json:"volumes,omitempty"`

	Services map[string]*Service `json:"services"`

	// BackupBeforeDeploy triggers an offen volume backup on the local
	// node before docker stack deploy. Failures never block the deploy
	// unless StrictBackup is also true.
	BackupBeforeDeploy bool `json:"backup_before_deploy,omitempty"`

	// StrictBackup, when true and BackupBeforeDeploy is also true, aborts
	// the deployment if the pre-deploy backup fails.  The default
	// (false) preserves the old best-effort behaviour.
	StrictBackup bool `json:"strict_backup,omitempty"`
}

// Service is one service declaration in an App manifest. The translator
// expands it into a rendered Compose service; each field below documents its
// own expansion.
type Service struct {
	// Image supports ${app}, ${env}, ${version}, ${registry}, ${env:VAR}.
	Image string `json:"image"`

	Replicas *int `json:"replicas,omitempty"`
	// RunOnce → restart_policy: condition: none. For migrations.
	RunOnce bool `json:"run_once,omitempty"`

	// Mode is the swarm deploy mode: "global" runs one task on every node
	// (used by platform agents like the OTel collector and backup); "" means
	// replicated (the default). Mutually exclusive with Replicas.
	Mode string `json:"mode,omitempty"`

	// Restart overrides the restart condition explicitly ("any",
	// "on-failure", "none"). Empty keeps the DSL defaults (run_once → none,
	// otherwise on-failure).
	Restart string `json:"restart,omitempty"`

	// RestartDelay sets the swarm restart_policy delay (e.g. "5s").
	// Platform agents (otel-collector, edge) use it to pace restarts.
	RestartDelay string `json:"restart_delay,omitempty"`

	// SkipFilelog excludes this service from the filelog receiver.
	// Set true when the service sends logs directly via OTLP (gRPC/HTTP).
	SkipFilelog bool `json:"skip_filelog,omitempty"`

	// Placement pins the service to nodes: "manager" or "worker" are role
	// constraints; any other value is treated as a node hostname and renders
	// the Swarm constraint node.hostname == <value> — the supported way to
	// pin a stateful service (with a volume) to one specific node so its
	// data never has to migrate. Empty means anywhere.
	Placement string `json:"placement,omitempty"`

	// Constraints appends RAW placement constraints verbatim (e.g.
	// "node.labels.pmcluster.storage == true" for storage-node-scoped
	// platform agents). Merged after the role/hostname constraint derived
	// from Placement.
	Constraints []string `json:"constraints,omitempty"`

	Command    []string `json:"command,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`

	Env     map[string]string `json:"env,omitempty"`
	Volumes []string          `json:"volumes,omitempty"`
	Secrets []string          `json:"secrets,omitempty"`

	// Binds are RAW host bind mounts emitted verbatim — NOT relocated under
	// the volume root (platform services mount docker.sock, /etc/localtime,
	// the volume root itself, etc.). Format: <host-path>:<container-path>[:ro].
	Binds []string `json:"binds,omitempty"`

	// Ports publishes container ports on the swarm (platform services only;
	// app stacks route through Traefik via expose).
	Ports []PortSpec `json:"ports,omitempty"`

	// Configs mounts Docker config FILES into the container. Each entry is a
	// config_path(<name>) expression — the shared syntax for both app and
	// platform stacks. The writer resolves <name> to the versioned swarm
	// config via its ConfigNames resolver and mounts it at the resolved path
	// (default /etc/<name>, overridable by the resolver).
	Configs []string `json:"configs,omitempty"`

	// ExtraHosts adds /etc/hosts entries (e.g. "host.docker.internal:host-gateway").
	ExtraHosts []string `json:"extra_hosts,omitempty"`

	// Resources sets cpu/memory reservations and limits.
	Resources *Resources `json:"resources,omitempty"`

	// User overrides the container user (e.g. "0:0").
	User string `json:"user,omitempty"`

	// Labels are RAW deploy labels merged onto the service (platform stacks
	// carry Traefik router labels etc. here). Standard auto-injected labels
	// (service/application/environment/version, io.pmcluster.*) always win on
	// key collisions.
	Labels map[string]string `json:"labels,omitempty"`

	// Logging sets the container logging driver + options (e.g. json-file
	// with max-size/max-file rotation). Empty keeps the swarm default.
	Logging *Logging `json:"logging,omitempty"`

	// Expose triggers Traefik label injection + traefik-net membership.
	Expose      *Expose      `json:"expose,omitempty"`
	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`
	Update      *Update      `json:"update,omitempty"`

	// DependsOn lists sibling services (by name) this service waits for.
	// Emitted as compose depends_on for compose parity; on the Swarm
	// backend (which ignores depends_on in v3) the waiting service keeps
	// restart_policy on-failure so it retries until the dependency is
	// healthy — the pattern for run-once migrations that must wait for
	// the database to accept connections.
	DependsOn []string `json:"depends_on,omitempty"`

	// Networks adds per-service EXTERNAL swarm network memberships beyond
	// the app-level Networks (merged). Platform stacks use it when a single
	// stack splits membership — e.g. observability: openobserve joins
	// traefik-net + monitoring-net while the otel-collector joins only
	// monitoring-net.
	Networks []string `json:"networks,omitempty"`
}

// PortSpec is a published port mapping. Target is the container port;
// Published is the swarm-side port (defaults to Target); Mode is ingress
// (default) or host. All swarm port publishing is TCP.
type PortSpec struct {
	Target    int    `json:"target"`
	Published int    `json:"published,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

// Resources is a cpu/memory reservation + limit pair (swarm deploy resources).
type Resources struct {
	Reservations *ResourceSpec `json:"reservations,omitempty"`
	Limits       *ResourceSpec `json:"limits,omitempty"`
}

// Logging sets the container logging driver and its options.
type Logging struct {
	Driver  string            `json:"driver,omitempty"`
	Options map[string]string `json:"options,omitempty"`
}

// ResourceSpec is one cpus/memory quantity. cpus is a decimal string (e.g.
// "0.25"); memory is a byte string (e.g. "128M", "512M").
type ResourceSpec struct {
	CPUs   string `json:"cpus,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// Expose routes a service through the cluster's Traefik ingress: it triggers
// the Traefik router labels and the traefik-net membership for the service.
type Expose struct {
	Port int    `json:"port"`
	Host string `json:"host"`

	// External publishes the exposed port to the swarm ingress network as a
	// real host port (target = Port, published = External, TCP). Empty means
	// the service is only reachable through Traefik (client → Traefik :443 →
	// service). Platform services that must be reachable directly (Traefik's
	// own :80/:443 ACME listener, the OTel collector's node-local :4318) set
	// this. When set and Host is empty, the port is published raw without any
	// Traefik router.
	External int `json:"external,omitempty"`

	// Mode is the swarm port publishing mode when External is set: ingress
	// (default, swarm load-balanced) or host (published on every node with a
	// task — the OTel collector uses host so the node-local daemon reaches
	// 127.0.0.1:4318 on the same node).
	Mode string `json:"mode,omitempty"`

	// Aliases are extra hostnames that route to the same backend. Each
	// alias emits its own Traefik router pointing at the same service.
	// Typically a customer's own domain (e.g. "idlibookfair.com") serving
	// the same app as the canonical ${domain} host. Supports ${app},
	// ${env}, ${domain}, etc. like Host.
	Aliases []string `json:"aliases,omitempty"`

	// CORSDisabled opts the exposed router out of the cluster-wide
	// cors-default Traefik middleware. Set this when the service owns
	// CORS itself (e.g. multi-tenant dynamic origins) or when its host
	// lives on a domain the cluster's shared regex doesn't cover.
	CORSDisabled bool `json:"cors_disabled,omitempty"`
}

// Healthcheck is either a shorthand (Type set) or a full-form Compose
// healthcheck (Test/Interval/Timeout/Retries set, Type empty).
type Healthcheck struct {
	// Type shortcuts:
	//   "pg_isready" → CMD-SHELL pg_isready -U $POSTGRES_USER -d $POSTGRES_DB
	//   "http"       → wget -q --spider http://127.0.0.1:<port>/<Path>
	Type string `json:"type,omitempty"`
	Path string `json:"path,omitempty"`

	Test        []string `json:"test,omitempty"`
	Interval    string   `json:"interval,omitempty"`
	Timeout     string   `json:"timeout,omitempty"`
	Retries     int      `json:"retries,omitempty"`
	StartPeriod string   `json:"start_period,omitempty"`
}

// Update is Swarm's rolling-update policy.
type Update struct {
	Parallelism int    `json:"parallelism,omitempty"`
	Delay       string `json:"delay,omitempty"`
	Order       string `json:"order,omitempty"`
}
