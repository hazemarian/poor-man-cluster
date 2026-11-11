package runners

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Spec is everything one runner container is created from. It is built fresh
// every (re)registration because the registration token in it is single-use.
//
// There is no compose document and no swarm service: a runner is a LOCAL
// container on the node it was selected for. A service would be one placement
// decision away from running a build on a manager, and its `--runnergroup`-scoped
// docker access would have to travel through an overlay network to reach the
// socket that belongs to a different node.
type Spec struct {
	Org        string
	Group      string
	RunnerName string
	Image      string
	Memory     string
	Token      string
	// SocketProxy mounts the platform's filtered docker socket and points
	// DOCKER_HOST at it. False is the documented debug fallback that mounts the
	// host's raw socket instead.
	SocketProxy bool
	SocketPath  string
}

// ContainerName is the local docker container name: the GitHub runner name, so
// `docker ps` and GitHub's runner list agree.
func (s Spec) ContainerName() string { return s.RunnerName }

// Argv is the `docker run` argument vector (without the leading "docker", and
// without the command word "run": the caller supplies both). It is a slice
// rather than a string so a token never passes through a shell.
//
// The mounts are the security decision:
//   - with the proxy (default), the runner gets ONLY the filtered socket at
//     SocketPath, and DOCKER_HOST points at it. It never sees /var/run/docker.sock.
//   - without it, the runner gets the host's raw socket: root on the node.
func (s Spec) Argv() ([]string, error) {
	if err := ValidateOrg(s.Org); err != nil {
		return nil, err
	}
	group := s.Group
	if strings.TrimSpace(group) == "" {
		group = "pmcluster"
	}
	if err := ValidateGroup(group); err != nil {
		return nil, err
	}
	if s.Token == "" {
		return nil, fmt.Errorf("create runner for org %s: registration token is empty", s.Org)
	}
	if s.RunnerName == "" {
		return nil, fmt.Errorf("create runner for org %s: runner name is empty", s.Org)
	}
	image := s.Image
	if image == "" {
		image = DefaultImage
	}
	memory := strings.TrimSpace(s.Memory)
	if memory == "" {
		memory = "2560m"
	}

	args := []string{
		"run", "-d",
		"--name", s.RunnerName,
		"--label", LabelRunner + "=true",
		"--label", LabelOrg + "=" + s.Org,
		"--label", LabelName + "=" + s.RunnerName,
		// A memory cap is not optional: at 1500m a govulncheck run was OOM-killed
		// mid-job, which looks like a CI flake rather than a platform limit.
		"--memory", memory,
		// Ephemeral by contract (EPHEMERAL=true): the container exits after one
		// job, and the reconcile creates a fresh one with a fresh token.
		"--restart", "no",
		// RUNNER_SCOPE is what makes ORG_NAME mean "the organisation" rather
		// than "a field nobody read": without it the image validates this as a
		// REPOSITORY runner, finds no REPO_URL, and exits 1 with
		// "Invalid configuration provided for url" — a crash loop that looks
		// like a platform failure and says nothing about the missing variable.
		"-e", "RUNNER_SCOPE=org",
		"-e", "ORG_NAME=" + s.Org,
		"-e", "RUNNER_GROUP=" + group,
		"-e", "RUNNER_TOKEN=" + s.Token,
		"-e", "RUNNER_NAME=" + s.RunnerName,
		"-e", "LABELS=" + Labels,
		"-e", "EPHEMERAL=true",
		"-e", "DISABLE_AUTO_UPDATE=true",
		"-e", "RUNNER_WORKDIR=/_work",
	}
	// Neutralise the image's pre-created docker-container builder so `docker
	// build` uses the daemon's own BuildKit. See EnvFixups.
	for _, kv := range s.EnvFixups() {
		args = append(args, "-e", kv)
	}
	if s.SocketProxy {
		path := s.SocketPath
		if strings.TrimSpace(path) == "" {
			path = "/run/pmcluster/docker.sock"
		}
		// Mount the DIRECTORY, not the socket file. The daemon recreates the
		// socket on every restart and a file bind-mount pins the OLD inode, so a
		// running runner loses the daemon the moment pmcluster restarts —
		// measured on a live runner: "Cannot connect to the Docker daemon at
		// unix:///run/pmcluster/docker.sock" after an upgrade, with the socket
		// present on the host. Same path inside and out keeps DOCKER_HOST a plain
		// unix path that also reads correctly in `docker inspect`.
		dir := filepath.Dir(path)
		args = append(args,
			"-e", "DOCKER_HOST=unix://"+path,
			"-v", dir+":"+dir,
		)
	} else {
		args = append(args,
			"-e", "DOCKER_HOST=unix:///var/run/docker.sock",
			"-v", "/var/run/docker.sock:/var/run/docker.sock",
		)
	}
	args = append(args, image)
	return args, nil
}

// EnvFixups are environment variables that correct a property of the RUNNER
// IMAGE which breaks builds on this platform, and which no workflow should have
// to work around.
//
// The image (myoung34/github-runner) ships a pre-created buildx builder named
// "default" using the **docker-container** driver. Every plain `docker build`
// therefore delegates to a BuildKit container, which the filtered socket cannot
// populate (PUT /containers/{id}/archive) nor exec into — measured on a live
// runner: `buildx ls` showed default* docker-container with STATUS error, and
// `buildx create --driver docker` is impossible because that driver is a
// singleton. Pointing BUILDX_CONFIG at a per-container empty directory makes the
// CLI use the daemon's own BuildKit (the docker driver) instead, which is what
// every workflow expects and what the proxy allows.
func (s Spec) EnvFixups() []string {
	return []string{"BUILDX_CONFIG=/tmp/pmcluster-buildx"}
}

// UsesRawSocket reports whether the spec mounts the host's docker socket.
// The doctor and the tests use it to state the exposure explicitly.
func (s Spec) UsesRawSocket() bool { return !s.SocketProxy }
