package runners

import (
	"fmt"
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
		"-e", "ORG_NAME=" + s.Org,
		"-e", "RUNNER_GROUP=" + group,
		"-e", "RUNNER_TOKEN=" + s.Token,
		"-e", "RUNNER_NAME=" + s.RunnerName,
		"-e", "LABELS=" + Labels,
		"-e", "EPHEMERAL=true",
		"-e", "DISABLE_AUTO_UPDATE=true",
		"-e", "RUNNER_WORKDIR=/_work",
	}
	if s.SocketProxy {
		path := s.SocketPath
		if strings.TrimSpace(path) == "" {
			path = "/run/pmcluster/docker.sock"
		}
		// Same path inside and out: DOCKER_HOST can then be a plain unix path
		// that also appears in the docs and in `docker inspect`.
		args = append(args,
			"-e", "DOCKER_HOST=unix://"+path,
			"-v", path+":"+path,
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

// UsesRawSocket reports whether the spec mounts the host's docker socket.
// The doctor and the tests use it to state the exposure explicitly.
func (s Spec) UsesRawSocket() bool { return !s.SocketProxy }
