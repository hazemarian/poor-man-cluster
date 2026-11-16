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
	// DinDImage overrides the docker:dind image. Pinned like every other image so
	// a node cannot silently change daemon version under a job.
	DinDImage string
	Memory    string
	Token     string
}

// CacheStores are the package caches a job is allowed to keep. They live in docker
// volumes that survive the job, because everything else about a runner does not: the
// container is ephemeral, and so is its daemon. Without them a Go build re-downloads
// ~150 MB of modules and an npm install re-fetches its tree on every single run — which
// is most of the difference between a three-minute job and an eight-minute one.
var CacheStores = []struct {
	Name string // volume suffix, and the mount point's name
	Path string // where it appears inside the runner
	Env  []string
}{
	{
		Name: "go",
		Path: "/go-cache",
		Env: []string{
			"GOMODCACHE=/go-cache/mod",
			"GOCACHE=/go-cache/build",
			"GOTOOLCHAIN=local",
		},
	},
	{
		Name: "npm",
		Path: "/npm-cache",
		Env: []string{
			"npm_config_cache=/npm-cache/npm",
			"YARN_CACHE_FOLDER=/npm-cache/yarn",
			"PNPM_HOME=/npm-cache/pnpm",
			"PNPM_STORE_DIR=/npm-cache/pnpm-store",
		},
	},
}

// CacheVolume is the volume that holds one cache store for one runner. Named after the
// runner, which already carries the node name, so two runners on two nodes cannot share
// a cache volume by accident.
func (s Spec) CacheVolume(store string) string {
	return s.RunnerName + "-cache-" + store
}

// DinDVolume is the volume the runner and its daemon share. It holds the inner socket
// and NOTHING else: it is removed with the job, so the next job cannot inherit a socket
// path whose owner is gone.
func (s Spec) DinDVolume() string { return s.RunnerName + "-docker" }

// DataVolume is the inner daemon's /var/lib/docker. It DOES survive the job — that is
// what makes image layers and the build cache reusable — and it is cleaned at every
// start rather than thrown away.
func (s Spec) DataVolume() string { return s.RunnerName + "-dind-data" }

// ContainerName is the local docker container name: the GitHub runner name, so
// `docker ps` and GitHub's runner list agree.
func (s Spec) ContainerName() string { return s.RunnerName }

// Argv is the `docker run` argument vector (without the leading "docker", and
// without the command word "run": the caller supplies both). It is a slice
// rather than a string so a token never passes through a shell.
//
// The mount is the security decision, and there are three modes:
//   - DinD (default): the runner shares a socket VOLUME with a privileged sibling
//     container running its own dockerd. It never sees /var/run/docker.sock, and a job
//     gets a real docker API.
//   - the filtered proxy (legacy, dind off): ONLY the socket at SocketPath, which
//     carries `docker build` and little else.
//   - the raw socket (debug fallback): root on the node.
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
	// A runner always gets a docker daemon of its own. There is no socket mode to
	// choose any more: the filtered proxy supported `docker build` and refused nearly
	// everything else, so every repository paid for its holes again in its own workflow
	// (compose, testcontainers, exec, networks). The host socket is never mounted.
	args = append(args,
		"-e", "DOCKER_HOST=unix:///var/run/docker.sock",
		"-v", s.DinDVolume()+":/var/run",
	)
	// Caches outlive the job; nothing else does.
	for _, c := range CacheStores {
		args = append(args, "-v", s.CacheVolume(c.Name)+":"+c.Path)
		for _, kv := range c.Env {
			args = append(args, "-e", kv)
		}
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

// DefaultDinDImage is pinned like every other image this platform runs: a node
// must not silently change daemon version under a job written against another.
const DefaultDinDImage = "docker:27-dind"

// DinDName is the privileged docker:dind container that hosts this runner's own
// daemon. It is removed together with the runner, so nothing it created survives.
func (s Spec) DinDName() string { return s.RunnerName + "-dind" }

// DinDArgv is the argument vector for the privileged docker:dind container that
// gives the job its own daemon.
//
// DOCKER_TLS_CERTDIR is emptied deliberately: with TLS on, the image boots dockerd
// with certificates and the client needs them, which is a second thing to plumb
// through a CI workflow that never asked for any of this. The socket is reachable
// only through a volume mounted into the runner container beside it.
func (s Spec) DinDArgv() ([]string, error) {
	// The dind container runs a DAEMON, not a runner: it must never inherit
	// Spec.Image, which is the runner image. That mistake shipped once and cost a
	// deploy — the privileged container booted a runner with no token, exited
	// immediately, and the failure read as "the dind daemon never answered".
	image := DefaultDinDImage
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
		// The socket volume is per-job; the DATA directory is per-node. Keeping
		// /var/lib/docker means the next job reuses the layers it just built and the
		// build cache that came with them, instead of pulling its base image again.
		"-v", s.DinDVolume() + ":/var/run",
		"-v", s.DataVolume() + ":/var/lib/docker",
	}
	if mem := strings.TrimSpace(s.Memory); mem != "" {
		// Bounds the WHOLE tree: dockerd, the runner's containers, builds and test
		// databases are all inside this cgroup.
		args = append(args, "--memory", mem)
	}
	args = append(args, image)
	return args, nil
}
