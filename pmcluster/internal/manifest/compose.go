package manifest

// composeFile is the top-level Docker Compose document.
type composeFile struct {
	Version  string                     `json:"version"`
	Services map[string]*composeService `json:"services,omitempty"`
	Volumes  map[string]*composeVolume  `json:"volumes,omitempty"`
	Networks map[string]*composeNetwork `json:"networks,omitempty"`
	Secrets  map[string]*composeSecret  `json:"secrets,omitempty"`
	Configs  map[string]*composeConfig  `json:"configs,omitempty"`
}

// composeService is the per-service block. Fields are an opinionated
// subset of the v3.9 spec — anything pmcluster doesn't emit is omitted.
type composeService struct {
	Image       string               `json:"image,omitempty"`
	Command     []string             `json:"command,omitempty"`
	Entrypoint  []string             `json:"entrypoint,omitempty"`
	Environment map[string]string    `json:"environment,omitempty"`
	Volumes     []string             `json:"volumes,omitempty"`
	Networks    []string             `json:"networks,omitempty"`
	Secrets     []string             `json:"secrets,omitempty"`
	Configs     []composeConfigMount `json:"configs,omitempty"`
	Ports       []composePort        `json:"ports,omitempty"`
	ExtraHosts  []string             `json:"extra_hosts,omitempty"`
	User        string               `json:"user,omitempty"`
	DependsOn   []string             `json:"depends_on,omitempty"`
	Healthcheck *composeHealthcheck  `json:"healthcheck,omitempty"`
	Logging     *composeLogging      `json:"logging,omitempty"`
	Deploy      *composeDeploy       `json:"deploy,omitempty"`
}

type composeLogging struct {
	Driver  string            `json:"driver,omitempty"`
	Options map[string]string `json:"options,omitempty"`
}

type composeConfigMount struct {
	Source string `json:"source,omitempty"`
	Target string `json:"target,omitempty"`
}

type composePort struct {
	Target    int    `json:"target,omitempty"`
	Published int    `json:"published,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

type composeHealthcheck struct {
	Test        []string `json:"test"`
	Interval    string   `json:"interval,omitempty"`
	Timeout     string   `json:"timeout,omitempty"`
	StartPeriod string   `json:"start_period,omitempty"`
	Retries     int      `json:"retries,omitempty"`
}

type composeDeploy struct {
	Mode          string                `json:"mode,omitempty"`
	Replicas      *int                  `json:"replicas,omitempty"`
	Labels        map[string]string     `json:"labels,omitempty"`
	Placement     *composePlacement     `json:"placement,omitempty"`
	RestartPolicy *composeRestartPolicy `json:"restart_policy,omitempty"`
	UpdateConfig  *composeUpdateConfig  `json:"update_config,omitempty"`
	Resources     *composeResources     `json:"resources,omitempty"`
}

type composeResources struct {
	Reservations *composeResourceSpec `json:"reservations,omitempty"`
	Limits       *composeResourceSpec `json:"limits,omitempty"`
}

type composeResourceSpec struct {
	CPUs   string `json:"cpus,omitempty"`
	Memory string `json:"memory,omitempty"`
}

type composePlacement struct {
	Constraints []string `json:"constraints,omitempty"`
}

type composeRestartPolicy struct {
	Condition   string `json:"condition,omitempty"`
	Delay       string `json:"delay,omitempty"`
	MaxAttempts int    `json:"max_attempts,omitempty"`
}

type composeUpdateConfig struct {
	Parallelism int    `json:"parallelism,omitempty"`
	Delay       string `json:"delay,omitempty"`
	Order       string `json:"order,omitempty"`
}

type composeVolume struct {
	Driver     string            `json:"driver,omitempty"`
	External   bool              `json:"external,omitempty"`
	DriverOpts map[string]string `json:"driver_opts,omitempty"`
}

type composeNetwork struct {
	Driver   string `json:"driver,omitempty"`
	External bool   `json:"external,omitempty"`
}

type composeSecret struct {
	External bool `json:"external,omitempty"`
	// Name overrides the swarm secret the compose key references. Set when a
	// secret has been rotated (swarm_rev > 1): the value lives in a NEW
	// versioned swarm secret (<name>_v<rev>) while the container mount path
	// stays /run/secrets/<logical-name> (BUG-007).
	Name string `json:"name,omitempty"`
}

type composeConfig struct {
	External bool `json:"external,omitempty"`
	// Name overrides the Docker config the compose key references. Set when
	// the config is versioned (pmcluster_otel_config_vN) — the container
	// mount path stays /etc/<logical-name>.
	Name string `json:"name,omitempty"`
}
