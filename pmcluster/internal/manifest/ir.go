package manifest

import "context"

// IR is the backend-neutral intermediate representation produced by DSL
// translation. It captures every semantic a deployment target needs —
// services, resolved env, volumes, secrets, healthchecks, replicas,
// placement, rollout policy and (optionally) public exposure — WITHOUT any
// target-specific syntax (no compose, no swarm labels, no k8s manifests).
//
// Writers (Writer) turn an IR into a concrete artifact: ComposeWriter emits
// Docker Compose v3.9 (the swarm backend, today), and future writers can
// emit Terraform / Helm / plain k8s YAML for other runtimes without touching
// the translator.
type IR struct {
	// Name is the app name (also derives the private network name).
	Name string

	// Env and Version are the environment / version labels stamped onto
	// every deployed workload.
	Env     string
	Version string

	// Platform marks a platform-managed stack (infra, edge, observability,
	// backup, sso). Writers stamp io.pmcluster.platform=true on every
	// service so the UI/CLI/down/--purge can differentiate them from user
	// app stacks.
	Platform bool

	// Networks are additional external swarm networks every service joins
	// (traefik-net / monitoring-net for platform stacks).
	Networks []string

	// Services is the ordered set of translated services.
	Services []IRService

	// Volumes is the set of named volumes the app's services reference
	// (auto-collected; writers relocate them under the volume root).
	Volumes []string

	// PlainVolumes are named volumes declared VERBATIM (not relocated under
	// the volume root). Platform stacks keep their stateful volumes exactly
	// where they are (openobserve_data, traefik_acme, pmui-data) so a
	// pipeline unification never strands or silently migrates existing data.
	PlainVolumes map[string]string

	// Secrets is the union of service- and app-level secret names.
	Secrets []string

	// Configs is the set of logical config names the app's services mount
	// (auto-collected; writers declare them external with versioned overrides).
	Configs []string
}

// IRService is one translated workload.
type IRService struct {
	Name       string
	Image      string
	Command    []string
	Entrypoint []string

	// Env holds the FULLY resolved environment (config() contents and
	// secrets() mount paths substituted).
	Env     map[string]string
	Volumes []string
	Secrets []string

	// Binds are RAW host bind mounts emitted verbatim (not relocated).
	Binds []string

	// Ports publishes container ports (platform services).
	Ports []IRPort

	// Configs mounts Docker config files (source logical name, target path).
	Configs []IRConfigMount

	// ExtraHosts adds /etc/hosts entries.
	ExtraHosts []string

	// Resources sets cpu/memory reservations/limits (nil = none).
	Resources *IRResources

	// User overrides the container user (empty = image default).
	User string

	// Labels are RAW deploy labels merged onto the service.
	Labels map[string]string

	// Logging is the container logging driver + options (nil = swarm default).
	Logging *IRLogging

	// Mode is the swarm deploy mode: "global" or "" (replicated).
	Mode string

	// Restart overrides the restart condition ("any", "on-failure", "none";
	// empty = DSL defaults).
	Restart string

	// RestartDelay is the swarm restart_policy delay (e.g. "5s").
	RestartDelay string

	// Constraints are RAW placement constraints appended verbatim.
	Constraints []string

	// ExtraNetworks are app-level external networks this service joins
	// (populated from IR.Networks at translate time).
	ExtraNetworks []string

	// Expose, when non-nil, marks the service as publicly reachable and
	// carries the routing intent (host, port, aliases, CORS).
	Expose *IRExpose

	// Healthcheck is the translated liveness probe (nil = none).
	Healthcheck *IRHealthcheck

	// RunOnce marks a one-shot job (restart policy none, no rollout).
	RunOnce bool

	// Replicas is the desired replica count for long-running services.
	Replicas int

	// Placement is the node-role constraint ("manager", "worker" or "").
	Placement string

	// Update is the rollout policy (nil = backend defaults).
	Update *IRUpdate

	// SkipFilelog opts the workload out of OTel filelog scraping.
	SkipFilelog bool

	// DependsOn lists sibling services this service must start AFTER — the
	// deploy pipeline topologically orders services into levels from this
	// and deploys level by level (waiting each level healthy before the
	// next). Emitted into compose for parity; the Swarm scheduler itself
	// ignores it.
	DependsOn []string
}

// IRPort is a published port mapping.
type IRPort struct {
	Target    int
	Published int
	Protocol  string
	Mode      string
}

// IRConfigMount mounts a Docker config file.
type IRConfigMount struct {
	Source string
	Target string
}

// IRResources is a cpu/memory reservation + limit pair.
type IRResources struct {
	Reservations *IRResourceSpec
	Limits       *IRResourceSpec
}

// IRLogging is the container logging driver + options.
type IRLogging struct {
	Driver  string
	Options map[string]string
}

// IRResourceSpec is one cpus/memory quantity.
type IRResourceSpec struct {
	CPUs   string
	Memory string
}

// IRExpose is the public-routing intent for a service.
type IRExpose struct {
	Port         int
	Host         string
	Aliases      []string
	CORSDisabled bool
}

// IRHealthcheck is the translated liveness probe:
//   - Type "pg_isready": wait for the container's own postgres.
//   - Type "http": GET http://127.0.0.1:<Expose.Port><Path>.
//   - Type "" (passthrough): Test is used verbatim.
type IRHealthcheck struct {
	Type        string
	Path        string
	Test        []string
	Interval    string
	Timeout     string
	Retries     int
	StartPeriod string
}

// IRUpdate is the rollout policy.
type IRUpdate struct {
	Parallelism int
	Delay       string
	Order       string
}

// IRRestartDelay is the swarm restart_policy delay string (e.g. "5s"),
// carried from the DSL RestartDelay field.

// Writer renders an IR into a deployment artifact. Each backend implements
// its own writer (ComposeWriter for swarm today; Terraform/Helm writers for
// other runtimes later). Writers must be deterministic: the same IR always
// yields the same bytes.
type Writer interface {
	Write(ctx context.Context, ir *IR) ([]byte, error)
}
