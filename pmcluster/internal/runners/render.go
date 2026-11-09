package runners

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// Spec is everything one runner container is rendered from. It is built fresh
// every (re)registration because the registration token in it is single-use.
type Spec struct {
	Repo        string
	RunnerName  string
	StackName   string
	Image       string
	Memory      string
	Token       string
	AllowLeader bool
}

// Render composes the one-service stack that runs one ephemeral runner.
//
// The three things this file is responsible for, each of which has bitten a
// hand-built runner:
//
//   - `--ephemeral` (EPHEMERAL=true): one job, then the container exits, so a
//     job can never inherit the previous job's workspace or credentials.
//   - the memory limit, as the operator set it (2560m by default) — a runner
//     under-provisioned at 1500m OOM-kills govulncheck halfway through.
//   - the leader exclusion constraint, unless the operator overrode it. It is
//     in the spec, so Swarm enforces it; a comment in a runbook does not.
func Render(spec Spec) ([]byte, error) {
	if err := ValidateRepo(spec.Repo); err != nil {
		return nil, err
	}
	if spec.Token == "" {
		return nil, fmt.Errorf("render runner for %s: registration token is empty", spec.Repo)
	}
	image := spec.Image
	if image == "" {
		image = DefaultImage
	}
	memory := strings.TrimSpace(spec.Memory)
	if memory == "" {
		memory = "2560m"
	}
	name := spec.RunnerName
	if name == "" {
		name = RunnerName("node", spec.Repo)
	}
	// The stack name is not part of the compose document: it is the argument
	// to `docker stack deploy`, and the caller owns it (StackName(repo)).
	doc := composeDoc{Services: map[string]composeService{
		"runner": {
			Image: image,
			Environment: map[string]string{
				// Repo-scoped registration: the token is minted by the App
				// installation and expires within the hour, which is why the
				// container is created fresh per job rather than restarted.
				"REPO_URL":            "https://github.com/" + spec.Repo,
				"RUNNER_TOKEN":        spec.Token,
				"RUNNER_NAME":         name,
				"LABELS":              Labels,
				"EPHEMERAL":           "true",
				"DISABLE_AUTO_UPDATE": "true",
				// The container builds images through the host socket, so it
				// must not try to run its own daemon or update itself.
				"RUNNER_WORKDIR": "/_work",
			},
			// Root-equivalent on the host: this is the documented risk, and
			// the reason only private repos are ever registered.
			Volumes: []composeMount{{
				Type:   "bind",
				Source: "/var/run/docker.sock",
				Target: "/var/run/docker.sock",
			}},
			Labels: map[string]string{
				LabelRunner: "true",
				LabelRepo:   spec.Repo,
				LabelName:   name,
			},
			// No `network_mode: host` and no published ports: the runner's
			// traffic leaves through the stack's own bridge network.
			Deploy: composeDeploy{
				Replicas: 1,
				RestartPolicy: composeRestart{
					Condition: "none",
				},
				Placement: composePlacement{
					Constraints: placementConstraints(spec.AllowLeader),
				},
				Resources: composeResources{
					Limits: composeLimits{Memory: memory},
				},
				// stop-first: two containers registering the same runner name
				// at once would make GitHub reject the second ("already
				// exists"), so never overlap a replacement task.
				UpdateConfig: &composeUpdate{Order: "stop-first", Parallelism: 1},
			},
		},
	}}
	return yaml.Marshal(doc)
}

// placementConstraints is the leader exclusion. Swarm evaluates `!=` as true
// for a node that does not carry the label at all, so workers without any
// pmcluster label still match — only the node labelled leader is excluded.
func placementConstraints(allowLeader bool) []string {
	if allowLeader {
		return nil
	}
	return []string{"node.labels.pmcluster.leader != true"}
}

type composeDoc struct {
	Services map[string]composeService `json:"services"`
}

type composeService struct {
	Image       string            `json:"image"`
	Environment map[string]string `json:"environment,omitempty"`
	Volumes     []composeMount    `json:"volumes,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Deploy      composeDeploy     `json:"deploy"`
}

type composeMount struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Target string `json:"target"`
}

type composeDeploy struct {
	Replicas      int              `json:"replicas"`
	RestartPolicy composeRestart   `json:"restart_policy"`
	Placement     composePlacement `json:"placement"`
	Resources     composeResources `json:"resources"`
	UpdateConfig  *composeUpdate   `json:"update_config,omitempty"`
}

type composeRestart struct {
	Condition string `json:"condition"`
}

type composePlacement struct {
	Constraints []string `json:"constraints,omitempty"`
}

type composeResources struct {
	Limits composeLimits `json:"limits"`
}

type composeLimits struct {
	Memory string `json:"memory,omitempty"`
}

type composeUpdate struct {
	Order       string `json:"order,omitempty"`
	Parallelism int    `json:"parallelism,omitempty"`
}
