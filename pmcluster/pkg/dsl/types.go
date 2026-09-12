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
	Name    string `json:"app"`     // stack name; also drives ${app} substitution
	Env     string `json:"env"`     // e.g. "production"
	Domain  string `json:"domain"`  // base domain; ${domain} in Expose.Host
	Version string `json:"version"` // image tag; ${version}; defaults to "latest"

	Registry string `json:"registry,omitempty"` // ${registry} for `${registry}/${app}:${version}`
	RepoURL  string `json:"repo_url,omitempty"` // metadata only; pmcluster never reads from git
	EnvFile  string `json:"env_file,omitempty"`

	// Secrets and Volumes are listed at App level so the translator can
	// emit the matching top-level blocks (secrets: external: true, etc.).
	Secrets []string `json:"secrets,omitempty"`
	Volumes []string `json:"volumes,omitempty"`

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

type Service struct {
	// Image supports ${app}, ${env}, ${version}, ${registry}, ${env:VAR}.
	Image string `json:"image"`

	Replicas *int `json:"replicas,omitempty"` // default 1; ignored when RunOnce
	// RunOnce → restart_policy: condition: none. For migrations.
	RunOnce bool `json:"run_once,omitempty"`

	// SkipFilelog excludes this service from the filelog receiver.
	// Set true when the service sends logs directly via OTLP (gRPC/HTTP).
	SkipFilelog bool `json:"skip_filelog,omitempty"`

	Placement string `json:"placement,omitempty"` // "manager" | "worker" | ""

	Command    []string `json:"command,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`

	Env     map[string]string `json:"env,omitempty"`
	Volumes []string          `json:"volumes,omitempty"` // "name:path" or "/host:/container"
	Secrets []string          `json:"secrets,omitempty"` // subset of App.Secrets

	// Expose triggers Traefik label injection + traefik-net membership.
	Expose      *Expose      `json:"expose,omitempty"`
	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`
	Update      *Update      `json:"update,omitempty"`
}

type Expose struct {
	Port int    `json:"port"` // container-side port
	Host string `json:"host"` // FQDN; e.g. "api.${app}.${domain}"

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
	Path string `json:"path,omitempty"` // for Type="http"; defaults to "/"

	Test     []string `json:"test,omitempty"`
	Interval string   `json:"interval,omitempty"`
	Timeout  string   `json:"timeout,omitempty"`
	Retries  int      `json:"retries,omitempty"`
}

// Update is Swarm's rolling-update policy.
type Update struct {
	Parallelism int    `json:"parallelism,omitempty"` // default 1
	Delay       string `json:"delay,omitempty"`       // default "10s"
	Order       string `json:"order,omitempty"`       // "start-first" (default) | "stop-first"
}
