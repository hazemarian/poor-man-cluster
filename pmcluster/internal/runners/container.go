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
	// DinDImage overrides the docker:dind image. Pinned like every other image so
	// a node cannot silently change daemon version under a job.
	DinDImage string
	Memory    string
	Token     string
	// SocketProxy mounts the platform's filtered docker socket and points
	// DOCKER_HOST at it. False is the documented debug fallback that mounts the
	// host's raw socket instead.
	SocketProxy bool
	SocketPath  string
	// DinD runs the job against a docker daemon of its OWN, inside a privileged
	// sibling container, sharing only a volume that holds the inner socket.
	//
	// This is the mode that makes CI ordinary: the job gets a real, unfiltered
	// docker API, so `docker compose up`, testcontainers, exec, networks and
	// volumes all work — none of which the filtered socket supports. The host
	// socket is never mounted, so a job still cannot touch the node's daemon, the
	// swarm, or another job's containers.
	//
	// It also closes a hole the memory work left open: the runner's cap bounded the
	// runner CONTAINER, while the daemon created build and test containers as its
	// SIBLINGS, outside that cgroup — so a `docker build` could still OOM the host
	// (measured: a 1839 MB node died repeatedly regardless of the 1200m cap). With
	// DinD every container the job creates lives INSIDE the capped container, so
	// one number finally bounds the whole tree.
	DinD bool
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
	switch {
	case s.DinD:
		// The runner shares the DinD container's socket volume: a real docker API
		// with nothing of the host in it. The host socket is NOT mounted, so the
		// swarm, the platform's own services and other jobs' containers stay out of
		// reach, and every container the job starts dies with this volume.
		args = append(args,
			"-e", "DOCKER_HOST=unix:///var/run/docker.sock",
			"-v", s.DinDVolume()+":/var/run",
		)
	case s.SocketProxy:
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
	default:
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
func (s Spec) UsesRawSocket() bool { return !s.SocketProxy && !s.DinD }

// DinDName is the privileged docker:dind container that hosts this runner's own
// daemon. It is removed together with the runner, so nothing it created survives.
func (s Spec) DinDName() string { return s.RunnerName + "-dind" }

// DinDVolume is the volume holding the inner daemon's socket, shared by the two
// containers and by nothing else. A volume rather than a bind mount: the socket
// must live somewhere the HOST filesystem does not own, and it must disappear with
// the job instead of lingering in /var/lib/docker/volumes forever.
func (s Spec) DinDVolume() string { return s.RunnerName + "-docker" }

// DinDArgv is the argument vector for the privileged docker:dind container that
// gives the job its own daemon.
//
// DOCKER_TLS_CERTDIR is emptied deliberately: with TLS on, the image boots dockerd
// with certificates and the client needs them, which is a second thing to plumb
// through a CI workflow that never asked for any of this. The socket is reachable
// only through a volume mounted into the runner container beside it.
func (s Spec) DinDArgv() ([]string, error) {
	if !s.DinD {
		return nil, fmt.Errorf("dind argv requested for %s, which is not a dind runner", s.RunnerName)
	}
	image := s.Image
	if strings.TrimSpace(s.DinDImage) != "" {
		image = s.DinDImage
	}
	args := []string{
		"run", "-d",
		"--name", s.DinDName(),
		// The labels are how the reconcile finds a leftover to remove. Without them
		// a crashed job leaves an orphan daemon holding disk and a port.
		"--label", LabelRunner + "=true",
		"--label", LabelName + "=" + s.RunnerName,
		"--label", "pmcluster.dind=true",
		// DinD needs a privileged container: it creates network namespaces,
		// mounts filesystems and talks to the kernel for every container the job
		// starts. That is the cost of a real docker API, and it is why this
		// container must hold nothing else.
		"--privileged",
		"-e", "DOCKER_TLS_CERTDIR=",
		"-v", s.DinDVolume() + ":/var/run",
	}
	if mem := strings.TrimSpace(s.Memory); mem != "" {
		// Bounds the WHOLE tree: dockerd, the runner's containers, builds and test
		// databases are all inside this cgroup.
		args = append(args, "--memory", mem)
	}
	args = append(args, image)
	return args, nil
}
