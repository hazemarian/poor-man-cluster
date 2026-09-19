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

	// Services is the ordered set of translated services.
	Services []IRService

	// Volumes is the set of named volumes the app's services reference
	// (auto-collected; writers relocate them under the volume root).
	Volumes []string

	// Secrets is the union of service- and app-level secret names.
	Secrets []string
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
	Type     string
	Path     string
	Test     []string
	Interval string
	Timeout  string
	Retries  int
}

// IRUpdate is the rollout policy.
type IRUpdate struct {
	Parallelism int
	Delay       string
	Order       string
}

// Writer renders an IR into a deployment artifact. Each backend implements
// its own writer (ComposeWriter for swarm today; Terraform/Helm writers for
// other runtimes later). Writers must be deterministic: the same IR always
// yields the same bytes.
type Writer interface {
	Write(ctx context.Context, ir *IR) ([]byte, error)
}
