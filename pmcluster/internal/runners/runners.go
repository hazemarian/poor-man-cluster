// Package runners manages the platform's self-hosted GitHub Actions runners:
// one ephemeral runner container per configured private repository, registered
// with a GitHub App installation token, reconciled by the control loop.
//
// Why the platform owns this at all: the org's GitHub-hosted minutes run out,
// and a repo-scoped self-hosted runner is the cheapest way to keep CI working.
// Why it is a first-class feature instead of a hand-run `docker run`: a runner
// left behind after the setting is turned off keeps polling GitHub and keeps
// holding a host-mounted docker socket (root-equivalent), which is exactly the
// kind of thing that must not depend on someone remembering a shell command.
//
// Three invariants this package enforces, each because breaking it is a
// security or cost incident rather than a bug:
//
//  1. Only PRIVATE repositories. A registration token is scoped to one repo,
//     and a public repository's fork PRs would run arbitrary workflow code on a
//     container with /var/run/docker.sock mounted. The reconcile refuses a
//     public repo instead of registering it.
//  2. No host networking. The runner reaches GitHub over the stack's own
//     bridge network; nothing on the host's network namespace is exposed.
//  3. Placement off the leader by default. Runners build images and compete
//     with the control plane for CPU; the constraint is in the rendered spec,
//     not a convention. A single-node cluster sets github_runner_allow_leader.
//
// Lifecycle is "ensure one running container per repo": each container runs
// with --ephemeral (one job, then exit). When it exits, the next reconcile mints
// a fresh registration token and redeploys the one-service stack, because a
// registration token is single-use and an hour-lived — it cannot be baked into
// a long-running service spec.
package runners

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// DefaultImage is the runner image. It reads REPO_URL / RUNNER_TOKEN /
// RUNNER_NAME / LABELS / EPHEMERAL from the environment, which is the contract
// the rendered stack relies on. Overridable per cluster only by changing this
// constant — deliberately not a setting: an arbitrary image here would run with
// the host's docker socket, so it is not an operator-editable knob.
const DefaultImage = "myoung34/github-runner:latest"

// Labels the runner registers with. self-hosted and linux come from GitHub;
// x64 and pmcluster are what a workflow selects on (`runs-on: [self-hosted,
// pmcluster]`), so a workflow never accidentally lands on a laptop runner.
const Labels = "self-hosted,linux,x64,pmcluster"

// RunnerNamePrefix marks every runner this platform registers. The prefix is
// reserved: de-registration removes the platform's runners in a repo by
// matching it, so a hand-made runner named pmc-* would be removed too.
const RunnerNamePrefix = "pmc-"

// StackNamePrefix names each repo's one-service stack. One stack per repo (not
// one stack with N services) so removing a repo removes exactly its runner and
// leaves the others' tasks alone.
const StackNamePrefix = "pmcluster-runner-"

// Service labels the reconcile uses to discover what exists. They are the
// "actual" half of desired-vs-actual: no table needs to be trusted for the
// containers, only for the de-registration bookkeeping.
const (
	LabelRunner = "pmcluster.runner"
	LabelRepo   = "pmcluster.runner.repo"
	LabelName   = "pmcluster.runner.name"
)

// Config is one reconcile's view of the runner settings.
type Config struct {
	Enabled     bool
	Repos       []string
	Memory      string
	AllowLeader bool
	// Node is the platform node the daemon runs on; it is the stable half of
	// the runner name (pmc-<node>-<repo-short>).
	Node string
	// Image overrides DefaultImage (tests, or a future per-cluster pin).
	Image string
}

// LoadConfig reads the runner settings from the store.
func LoadConfig(ctx context.Context, st *store.Store, node string) Config {
	cfg := Config{
		Enabled:     cluster.LoadGitHubRunnersEnabled(ctx, st),
		Repos:       ParseRepos(settingValue(ctx, st, cluster.SettingGitHubRunnerRepos())),
		Memory:      cluster.LoadGitHubRunnerMemory(ctx, st),
		AllowLeader: cluster.LoadGitHubRunnerAllowLeader(ctx, st),
		Node:        node,
		Image:       DefaultImage,
	}
	return cfg
}

func settingValue(ctx context.Context, st *store.Store, key string) string {
	if st == nil {
		return ""
	}
	return st.SettingDefault(ctx, key, "")
}

// ParseRepos splits the comma-separated github_runner_repos value into a
// deduped, order-preserving list. Comma-separated is the chosen shape (not a
// repeated flag) because the same string is the console's single text field,
// the `cluster settings set` value, and the setup wizard's answer.
func ParseRepos(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

// NormalizeRepos validates every entry and returns the canonical
// comma-separated form (trimmed, deduped, order preserved). An invalid entry
// fails the whole value: silently dropping a typo would leave a repo the
// operator believes has a runner with none, and nothing would say so.
func NormalizeRepos(raw string) (string, error) {
	repos := ParseRepos(raw)
	for _, r := range repos {
		if err := ValidateRepo(r); err != nil {
			return "", err
		}
	}
	return strings.Join(repos, ","), nil
}

var reRepoPart = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateRepo accepts only owner/name. The value ends up in API paths and in
// a stack name, so a path-like or spaced value is refused rather than escaped.
func ValidateRepo(repo string) error {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("repository must be owner/name, got %q", repo)
	}
	for _, p := range parts {
		if p == "." || p == ".." || !reRepoPart.MatchString(p) {
			return fmt.Errorf("repository %q is not a valid owner/name (letters, digits, '.', '_' and '-' only)", repo)
		}
	}
	return nil
}

var reMemory = regexp.MustCompile(`(?i)^[0-9]+(\.[0-9]+)?[bkmg]?$`)

// ValidateMemory accepts a Docker memory quantity: a positive number with an
// optional b/k/m/g suffix ("2560m", "3g", "1073741824"). Docker's own parser
// rejects anything else at deploy time, hours after the setting was saved, so
// the setting surface refuses it up front.
func ValidateMemory(v string) error {
	s := strings.TrimSpace(v)
	if s == "" {
		return fmt.Errorf("memory limit must be a Docker quantity such as 2560m or 3g")
	}
	if !reMemory.MatchString(s) {
		return fmt.Errorf("memory limit %q is not a Docker quantity (a positive number with an optional b/k/m/g suffix, e.g. 2560m)", v)
	}
	num := strings.TrimRight(s, "bkmgBKMGH")
	if n, err := strconv.ParseFloat(strings.TrimSpace(num), 64); err != nil || n <= 0 {
		return fmt.Errorf("memory limit %q must be greater than zero", v)
	}
	return nil
}

// RepoShort is the repo's stable, filesystem/stack-safe short name:
// owner-name-<6 hex>. The hash keeps two repos whose owners differ only in
// characters that sanitize to '-' from colliding.
func RepoShort(repo string) string {
	slug := strings.ToLower(repo)
	slug = strings.ReplaceAll(slug, "/", "-")
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, slug)
	slug = strings.Trim(slug, "-._")
	sum := sha256.Sum256([]byte(repo))
	if slug == "" {
		slug = "repo"
	}
	short := slug + "-" + hex.EncodeToString(sum[:])[:6]
	if len(short) > 48 {
		short = short[:48]
	}
	return short
}

// RunnerName is the GitHub-visible runner name: pmc-<node>-<repo-short>,
// capped at GitHub's 64-character runner-name limit.
func RunnerName(node, repo string) string {
	node = strings.TrimSpace(node)
	if node == "" {
		node = "node"
	}
	name := RunnerNamePrefix + sanitizeName(node) + "-" + RepoShort(repo)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// StackName is the one-service Swarm stack for a repo.
func StackName(repo string) string { return StackNamePrefix + RepoShort(repo) }

// sanitizeName lowercases a hostname into the runner-name-safe alphabet.
func sanitizeName(s string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, strings.TrimSpace(s))
	return strings.Trim(out, "-")
}

// SortRepos returns the repo list in a deterministic order for logs, reports
// and the rendered plan.
func SortRepos(repos []string) []string {
	out := append([]string(nil), repos...)
	sort.Strings(out)
	return out
}
