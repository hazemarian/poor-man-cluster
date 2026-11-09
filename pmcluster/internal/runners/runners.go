// Package runners manages the platform's self-hosted GitHub Actions runners:
// ONE ephemeral runner container per node, registered at ORG level into a
// GitHub runner group, reconciled by the control loop.
//
// Why org-level and not per-repo: one registration serves every repository the
// runner group selects, so a new repository does not need a new runner (and a
// removed one does not need a runner torn down). Org-level registration is also
// what the operator's hand-built runner already does, so the platform's view and
// reality line up.
//
// Why it is a first-class feature instead of a hand-run `docker run`: a runner
// left behind after the setting is turned off keeps polling GitHub and keeps
// holding a host-mounted docker socket (root-equivalent), which is exactly the
// kind of thing that must not depend on someone remembering a shell command.
//
// Three invariants this package enforces, each because breaking it is a
// security or cost incident rather than a bug:
//
//  1. The runner group's visibility is `selected`, NEVER `all`. A runner in an
//     all-visible group executes workflow code from any repository in the
//     organisation, including one an attacker can push to — on a container that
//     has the host's docker socket. The group is created `selected` and an
//     existing `all` group is refused by name.
//  2. No host networking. The runner reaches GitHub over the stack's own
//     bridge network; nothing on the host's network namespace is exposed.
//  3. Placement off the leader by default. Runners build images and compete
//     with the control plane for CPU; the constraint is in the rendered spec,
//     not a convention. A single-node cluster sets github_runner_allow_leader.
//
// Lifecycle is "one running container": it runs with --ephemeral (one job, then
// exit). When it exits, the next reconcile mints a fresh registration token and
// redeploys the one-service stack, because a registration token is single-use
// and an hour-lived — it cannot be baked into a long-running service spec.
//
// The feature switch is the ONLY thing that decides whether a runner exists.
// Every surface (setup wizard, `cluster settings`, console, doctor, reconcile)
// reads github_runners_enabled; nothing derives its own opinion. Turning it off
// tears down: containers removed by their `pmcluster.runner` label, registrations
// deleted by the name on that label (or the one in the bookkeeping), rows
// cleared. It is idempotent, and a GitHub outage does not stop the containers
// from coming off the node.
package runners

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// DefaultImage is the runner image. It reads ORG_NAME / RUNNER_GROUP /
// RUNNER_TOKEN / RUNNER_NAME / LABELS / EPHEMERAL from the environment, which is
// the contract the rendered stack relies on. Overridable per cluster only by
// changing this constant — deliberately not a setting: an arbitrary image here
// would run with the host's docker socket, so it is not an operator-editable
// knob.
const DefaultImage = "myoung34/github-runner:latest"

// Labels the runner registers with. self-hosted and linux come from GitHub;
// x64 and pmcluster are what a workflow selects on (`runs-on: [self-hosted,
// pmcluster]`), so a workflow never accidentally lands on a laptop runner.
const Labels = "self-hosted,linux,x64,pmcluster"

// RunnerNamePrefix marks every runner this platform registers. The prefix is
// reserved: it is how a stray platform runner is recognised (and reported) in
// the org's runner list.
const RunnerNamePrefix = "pmc-"

// StackNamePrefix names the runner container. Kept for the legacy prefix check
// only: a container named with it is one this platform used to create.
const StackNamePrefix = "pmcluster-runner-"

// Service labels the reconcile uses to discover what exists — and, just as
// importantly, to know what the platform OWNS: teardown removes only services
// carrying LabelRunner=true, never a container someone started by hand.
const (
	LabelRunner = "pmcluster.runner"
	LabelOrg    = "pmcluster.runner.org"
	LabelName   = "pmcluster.runner.name"
)

// Config is one reconcile's view of the runner settings.
type Config struct {
	Enabled bool
	// Org is the GitHub organisation the runner registers into.
	Org string
	// Group is the org runner group the runner joins.
	Group string
	// Nodes are the hostnames allowed to run a runner, in the order the
	// operator picked them. Empty with Enabled=true is a refusal: a feature
	// that is on but runs nowhere is the "half-enabled" state this setting
	// exists to make impossible.
	Nodes []string
	// SocketProxy makes runners talk to the platform's filtered docker socket
	// instead of the host's real one. Default true; the raw mount is a
	// debugging fallback and is documented as unsafe for untrusted workflows.
	SocketProxy bool
	// SocketPath is the filtered socket the runner mounts.
	SocketPath  string
	Memory      string
	AllowLeader bool
	// Node is THIS daemon's hostname: the node that decides whether it runs a
	// runner, and the stable half of the runner name (pmc-<node>-org).
	Node string
	// Image overrides DefaultImage (tests, or a future per-cluster pin).
	Image string
}

// LoadConfig reads the runner settings from the store.
func LoadConfig(ctx context.Context, st *store.Store, node string) Config {
	return Config{
		Enabled:     cluster.LoadGitHubRunnersEnabled(ctx, st),
		Org:         cluster.LoadGitHubRunnerOrg(ctx, st),
		Group:       cluster.LoadGitHubRunnerGroup(ctx, st),
		Nodes:       ParseNodes(cluster.LoadGitHubRunnerNodes(ctx, st)),
		SocketProxy: cluster.LoadGitHubRunnerSocketProxy(ctx, st),
		SocketPath:  cluster.LoadGitHubRunnerSocketPath(ctx, st),
		Memory:      cluster.LoadGitHubRunnerMemory(ctx, st),
		AllowLeader: cluster.LoadGitHubRunnerAllowLeader(ctx, st),
		Node:        node,
		Image:       DefaultImage,
	}
}

// WatchesNode reports whether this daemon's node is one of the selected runner
// nodes (compared case-insensitively: Swarm hostnames are lowercased, an
// operator's list need not be).
func (c Config) WatchesNode() bool {
	for _, n := range c.Nodes {
		if strings.EqualFold(strings.TrimSpace(n), strings.TrimSpace(c.Node)) {
			return true
		}
	}
	return false
}

// ParseNodes splits the comma-separated github_runner_nodes value into a
// deduped, order-preserving list of hostnames.
func ParseNodes(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key := strings.ToLower(part)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, part)
	}
	return out
}

var reHostname = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,252}[A-Za-z0-9])?$`)

// ValidateNodes checks every entry as a hostname and refuses an empty list. The
// role check (no managers, no leader) needs the live node list, so it happens in
// the reconcile and in the console's picker; this is the shape check every
// surface can make.
func ValidateNodes(raw string) error {
	nodes := ParseNodes(raw)
	if len(nodes) == 0 {
		return fmt.Errorf("github_runner_nodes is required when runners are enabled: list the worker hostnames that may run one (a runner is a local container on the node it is listed for)")
	}
	for _, n := range nodes {
		if !reHostname.MatchString(n) {
			return fmt.Errorf("%q is not a hostname", n)
		}
	}
	return nil
}

// NormalizeNodes returns the canonical comma-separated form (trimmed, deduped,
// order preserved). An invalid entry fails the whole value: silently dropping a
// typo would leave a node the operator believes runs a runner with none.
func NormalizeNodes(raw string) (string, error) {
	if err := ValidateNodes(raw); err != nil {
		return "", err
	}
	return strings.Join(ParseNodes(raw), ","), nil
}

var reOrg = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

// ValidateOrg accepts a GitHub organisation login: letters, digits and single
// hyphens, not starting or ending with one (GitHub's own rule). The value ends
// up in API paths and in the group's name, so a path-like or spaced value is
// refused rather than escaped.
func ValidateOrg(org string) error {
	if strings.TrimSpace(org) == "" {
		return fmt.Errorf("github_runner_org is required: an org-level runner registers into an organisation (e.g. acme)")
	}
	if !reOrg.MatchString(org) {
		return fmt.Errorf("organisation %q is not a valid GitHub org login (letters, digits and inner '-' only)", org)
	}
	return nil
}

var reGroup = regexp.MustCompile(`^[A-Za-z0-9._ -]{1,64}$`)

// ValidateGroup accepts a runner group name. GitHub allows letters, digits,
// spaces, '.', '_' and '-' up to 64 characters; anything else is refused here
// rather than by the API hours later.
func ValidateGroup(group string) error {
	if strings.TrimSpace(group) == "" {
		return fmt.Errorf("github_runner_group must not be blank (default: %s)", cluster.DefaultGitHubRunnerGroup)
	}
	if !reGroup.MatchString(group) {
		return fmt.Errorf("runner group %q is not a valid GitHub group name (letters, digits, space, '.', '_' and '-', at most 64 characters)", group)
	}
	return nil
}

// NormalizeOrg trims and validates an organisation.
func NormalizeOrg(raw string) (string, error) {
	org := strings.TrimSpace(raw)
	if err := ValidateOrg(org); err != nil {
		return "", err
	}
	return org, nil
}

// NormalizeGroup trims and validates a runner group; blank means the default.
func NormalizeGroup(raw string) (string, error) {
	group := strings.TrimSpace(raw)
	if group == "" {
		group = cluster.DefaultGitHubRunnerGroup
	}
	if err := ValidateGroup(group); err != nil {
		return "", err
	}
	return group, nil
}

// ValidateMemory accepts a Docker memory quantity: a positive number with an
// optional b/k/m/g suffix ("2560m", "3g", "1073741824"). Docker's own parser
// rejects anything else at deploy time, hours after the setting was saved, so
// the setting surface refuses it up front.
var reMemory = regexp.MustCompile(`(?i)^[0-9]+(\.[0-9]+)?[bkmg]?$`)

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

// NodeShort is a node hostname's filesystem/stack-safe form, with a short hash
// so two hostnames that sanitize to the same string stay distinct stacks.
func NodeShort(node string) string {
	slug := sanitizeName(node)
	if slug == "" {
		slug = "node"
	}
	sum := sha256.Sum256([]byte(node))
	short := slug + "-" + hex.EncodeToString(sum[:])[:6]
	if len(short) > 40 {
		short = short[:40]
	}
	return short
}

// RunnerName is the GitHub-visible runner name: pmc-<node>-org, capped at
// GitHub's 64-character runner-name limit. The -org suffix says the scope, so a
// runner in the org's list is unambiguous next to a hand-made one.
func RunnerName(node string) string {
	slug := sanitizeName(strings.TrimSpace(node))
	if slug == "" {
		slug = "node"
	}
	if len(slug) > 52 {
		slug = slug[:52]
	}
	name := RunnerNamePrefix + slug + "-org"
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// StackName is the one-service Swarm stack that runs this node's runner.
func StackName(node string) string { return StackNamePrefix + NodeShort(node) }

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
