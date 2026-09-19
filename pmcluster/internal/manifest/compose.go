package manifest

// composeFile is the top-level Docker Compose document.
type composeFile struct {
	Version  string                     `json:"version"`
	Services map[string]*composeService `json:"services,omitempty"`
	Volumes  map[string]*composeVolume  `json:"volumes,omitempty"`
	Networks map[string]*composeNetwork `json:"networks,omitempty"`
	Secrets  map[string]*composeSecret  `json:"secrets,omitempty"`
}

// composeService is the per-service block. Fields are an opinionated
// subset of the v3.9 spec — anything pmcluster doesn't emit is omitted.
type composeService struct {
	Image       string              `json:"image,omitempty"`
	Command     []string            `json:"command,omitempty"`
	Entrypoint  []string            `json:"entrypoint,omitempty"`
	Environment map[string]string   `json:"environment,omitempty"`
	Volumes     []string            `json:"volumes,omitempty"`
	Networks    []string            `json:"networks,omitempty"`
	Secrets     []string            `json:"secrets,omitempty"`
	Healthcheck *composeHealthcheck `json:"healthcheck,omitempty"`
	Deploy      *composeDeploy      `json:"deploy,omitempty"`
}

type composeHealthcheck struct {
	Test     []string `json:"test"`
	Interval string   `json:"interval,omitempty"`
	Timeout  string   `json:"timeout,omitempty"`
	Retries  int      `json:"retries,omitempty"`
}

type composeDeploy struct {
	Mode          string                `json:"mode,omitempty"`
	Replicas      *int                  `json:"replicas,omitempty"`
	Labels        map[string]string     `json:"labels,omitempty"`
	Placement     *composePlacement     `json:"placement,omitempty"`
	RestartPolicy *composeRestartPolicy `json:"restart_policy,omitempty"`
	UpdateConfig  *composeUpdateConfig  `json:"update_config,omitempty"`
}

type composePlacement struct {
	Constraints []string `json:"constraints,omitempty"`
}

type composeRestartPolicy struct {
	Condition string `json:"condition,omitempty"`
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
}
