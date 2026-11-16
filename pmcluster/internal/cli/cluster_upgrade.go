package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/upgrade"
)

// clusterUpgradeCmd replaces the pmcluster binary on every node — as a workflow, with a gate and
// a rollback, because this is the one operation that runs unattended and can take a whole cluster
// down.
//
// It replaces hack/upgrade-nodes.sh, which did the ordering and the checksum correctly and then
// slept six seconds and printed `systemctl is-active` — a value that reads "active" for a binary
// that crash-loops. Nothing verified that the new daemon answered, and nothing rolled back, so a
// bad release reported success across the fleet.
var clusterUpgradeCmd = &cobra.Command{
	Use:   "upgrade <version>",
	Short: "Replace the pmcluster binary on every node, with a health gate and rollback",
	Long: `Upgrades the binary across the swarm, in the order that keeps the cluster serving:
workers, then follower managers, then the LEADER last.

Every node is verified before the next one is touched. After the restart the workflow waits for
/health to answer with the NEW version in its body (` + "`systemctl is-active`" + ` says "active" while a
binary crash-loops, which is why the old script could not see a bad release). A node that does
not come up healthy is rolled back to the binary it was running before, and the rollout STOPS
there — the nodes it had not reached are left alone.

The rollout is idempotent: a node already on the target version is skipped, so a failed run is
resumed by running the same command again.

Nodes are addressed by the addresses you give it (--nodes, or PMCLUSTER_NODES). The swarm reports
internal addresses, which are not the ones you can reach, so guessing them is how an upgrade
lands on the wrong host.`,
	Example: `  pmcluster cluster upgrade v0.2.347 --nodes 1.2.3.4,5.6.7.8 --key ~/.ssh/pmcluster_ed25519
  PMCLUSTER_NODES=... PMCLUSTER_SSH_USER=root pmcluster cluster upgrade v0.2.347 --dry-run`,
	Args: cobra.ExactArgs(1),
	RunE: runClusterUpgrade,
}

func init() {
	f := clusterUpgradeCmd.Flags()
	f.String("nodes", os.Getenv("PMCLUSTER_NODES"), "comma-separated addresses (default: $PMCLUSTER_NODES)")
	f.String("key", os.Getenv("PMCLUSTER_SSH_KEY"), "ssh private key (default: $PMCLUSTER_SSH_KEY, else ~/.ssh/pmcluster_ed25519)")
	f.String("user", envOr("PMCLUSTER_SSH_USER", "root"), "ssh user (needs sudo -n on the node)")
	f.String("arch", "amd64", "artifact architecture")
	f.String("repo", "hazemarian/poor-man-cluster", "release repository")
	f.Bool("dry-run", false, "print the plan and touch nothing")
	f.Bool("no-rollback", false, "do not restore the previous binary on a failed health gate")
	f.Duration("health-timeout", 90*time.Second, "how long to wait for /health after a restart")
	clusterCmd.AddCommand(clusterUpgradeCmd)
}

func runClusterUpgrade(cmd *cobra.Command, args []string) error {
	defer initCLITelemetry()()
	version := strings.TrimSpace(args[0])
	out := cmd.OutOrStdout()

	hostsFlag, _ := cmd.Flags().GetString("nodes")
	hosts := splitHostList(hostsFlag)
	if len(hosts) == 0 {
		return fmt.Errorf("no nodes given: pass --nodes or set PMCLUSTER_NODES (the swarm reports internal addresses, which a rollout cannot dial)")
	}
	key, _ := cmd.Flags().GetString("key")
	if key == "" {
		key = filepath.Join(os.Getenv("HOME"), ".ssh", "pmcluster_ed25519")
	}
	user, _ := cmd.Flags().GetString("user")
	arch, _ := cmd.Flags().GetString("arch")
	repo, _ := cmd.Flags().GetString("repo")

	// 1. The artifact, fetched and verified HERE, then pushed. The release repository is
	// private, so a node cannot fetch it without a second credential; copying the bytes over
	// the channel the rollout already authenticates keeps one credential per fleet.
	fmt.Fprintf(out, "▶ release %s (%s)\n", version, arch)
	dir, err := os.MkdirTemp("", "pmcluster-upgrade-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	asset := fmt.Sprintf("pmcluster-%s-linux-%s.tar.gz", version, arch)
	if err := ghReleaseDownload(cmd.Context(), repo, version, dir); err != nil {
		return err
	}
	artifact := filepath.Join(dir, asset)
	want, err := checksumFromSums(filepath.Join(dir, "SHA256SUMS.txt"), asset)
	if err != nil {
		return err
	}
	got, err := sha256File(artifact)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s checksum is %s, the release says %s — refusing to install a binary that does not match its manifest", asset, got, want)
	}
	fmt.Fprintf(out, "  checksum verified (%s)\n", want[:16])

	// 2. Ask each node what it is. The role and the leader flag come from the NODE, not from a
	// list typed on a laptop: that is what makes "leader last" true rather than aspirational.
	tr := &sshTransport{Key: key, User: user}
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
	defer cancel()

	targets := make([]upgrade.Target, 0, len(hosts))
	fmt.Fprintf(out, "▶ reading each node's role\n")
	for _, h := range hosts {
		t := upgrade.Target{Host: h, User: user}
		// A WORKER cannot run `docker node inspect self` ("This node is not a swarm
		// manager"), so asking for the role that way fails on exactly the nodes a rollout
		// starts with. `docker info` answers on both: ControlAvailable is the manager test.
		probe, err := tr.Run(ctx, t, strings.Join([]string{
			"set -e",
			`if docker info --format '{{.Swarm.ControlAvailable}}' 2>/dev/null | grep -qi true; then`,
			`  docker node inspect self --format '{{.Spec.Role}}|{{.ManagerStatus.Leader}}'`,
			"else",
			`  echo 'worker|false'`,
			"fi",
		}, "\n"))
		if err != nil {
			return fmt.Errorf("%s: %w (a node the rollout cannot identify is a node it refuses to touch)", h, err)
		}
		parts := strings.Split(strings.TrimSpace(probe), "|")
		t.Role = parts[0]
		t.Leader = len(parts) > 1 && strings.EqualFold(parts[1], "true")
		role := t.Role
		if t.Leader {
			role += " (leader)"
		}
		fmt.Fprintf(out, "  %-16s %s\n", h, role)
		targets = append(targets, t)
	}

	noRollback, _ := cmd.Flags().GetBool("no-rollback")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	timeout, _ := cmd.Flags().GetDuration("health-timeout")

	rep, err := upgrade.Run(ctx, upgrade.Options{
		Version:       version,
		Artifact:      artifact,
		SHA256:        want,
		Targets:       targets,
		Transport:     tr,
		HealthTimeout: timeout,
		NoRollback:    noRollback,
		DryRun:        dryRun,
		Logf: func(format string, a ...any) {
			fmt.Fprintf(out, format+"\n", a...)
		},
	})
	if err != nil {
		return err
	}

	fmt.Fprintln(out)
	printUpgradeReport(out, rep)
	if rep.Failed() || rep.Aborted {
		return fmt.Errorf("the rollout did not finish: %d node(s) attempted, see above", len(rep.Nodes))
	}
	return nil
}

func printUpgradeReport(out io.Writer, rep upgrade.Report) {
	fmt.Fprintf(out, "  Upgraded to   : %s\n", rep.Version)
	for _, n := range rep.Nodes {
		state := n.To
		switch {
		case n.Skipped:
			state = n.From + " (already)"
		case n.RolledBack:
			state = n.From + " (rolled back)"
		case n.Err != "":
			state = "FAILED"
		}
		fmt.Fprintf(out, "  %-16s %-12s %s\n", n.Target.Host, n.From, state)
		if n.Err != "" {
			fmt.Fprintf(out, "  %-16s ✖ %s\n", "", n.Err)
		}
	}
	if rep.Aborted {
		fmt.Fprintln(out, "  Rollout       : STOPPED at the first failure; later nodes were not touched")
	}
}

// splitHostList tolerates spaces, commas and repeated commas — the flag is typed by a human at
// 3am and a stray separator must not silently drop a node.
func splitHostList(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// ghReleaseDownload fetches the artifact and SHA256SUMS with the gh CLI, which the operator
// already has (every release in this project is published with it).
func ghReleaseDownload(ctx context.Context, repo, version, dir string) error {
	args := []string{"release", "download", version, "-R", repo,
		"-p", "*.tar.gz", "-p", "SHA256SUMS.txt", "-D", dir, "--clobber"}
	c := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("download release %s: %v: %s", version, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// checksumFromSums reads the expected sha256 for asset out of SHA256SUMS.txt (`<sha>  <name>`,
// with an optional `*` binary marker that some tools write).
func checksumFromSums(path, asset string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read SHA256SUMS.txt: %w", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == asset {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS.txt has no entry for %s", asset)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sshTransport runs the workflow's scripts over ssh, as root (directly or through sudo -n).
//
// The transport is the only part of the upgrade that knows ssh exists: the ordering, the gate and
// the rollback are in internal/upgrade and are unit-tested against a fake. When a node can be
// asked to replace its own binary over the API it already serves, this type is the thing that
// changes.
type sshTransport struct {
	Key  string
	User string
}

func (s *sshTransport) Run(ctx context.Context, t upgrade.Target, script string) (string, error) {
	user := t.User
	if user == "" {
		user = s.User
	}
	c := exec.CommandContext(ctx, "ssh",
		"-i", s.Key, "-o", "BatchMode=yes", "-o", "ConnectTimeout=15",
		"-o", "StrictHostKeyChecking=accept-new",
		user+"@"+t.Host, "sudo -n bash -s")
	c.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Put copies a file to the node through the same authenticated channel — no second credential,
// and no assumption that the node can reach the release repository.
func (s *sshTransport) Put(ctx context.Context, t upgrade.Target, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	user := t.User
	if user == "" {
		user = s.User
	}
	c := exec.CommandContext(ctx, "ssh",
		"-i", s.Key, "-o", "BatchMode=yes", "-o", "ConnectTimeout=15",
		"-o", "StrictHostKeyChecking=accept-new",
		user+"@"+t.Host, fmt.Sprintf("sudo -n sh -c 'cat > %s && chmod 0600 %s'", remotePath, remotePath))
	c.Stdin = f
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("copy to %s: %v: %s", remotePath, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
