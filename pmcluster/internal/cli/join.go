package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// nodeNameRe mirrors the manifest placement-pin validation: a Docker node
// hostname or node ID — letters, digits, dots, dashes, underscores.
var nodeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// --- overridable dependencies for the registry-credential helpers ------------
// (package vars, same pattern as hostOS/systemctlFn/writeUnitFn in daemon.go,
// so tests can fake the network calls without a real ssh/docker).

// dockerConfigPathFn resolves the local Docker config file. Overridable so
// the credential helpers write into a temp dir under test.
var dockerConfigPathFn = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

// sshRemoteCatFn reads a file from a remote host over ssh:
//
//	ssh root@<host> 'cat /root/.docker/config.json'
//
// (the ssh-cat form — no scp dependency, only ssh). Overridable so tests can
// fake the manager's config without a network.
var sshRemoteCatFn = func(ctx context.Context, host, remotePath string) ([]byte, error) {
	c := exec.CommandContext(ctx, "ssh", "-o", "StrictHostKeyChecking=accept-new",
		"root@"+host, "cat "+remotePath)
	out, err := c.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// dockerPullFn runs 'docker pull <image>'. Overridable so tests never touch a
// real Docker daemon.
var dockerPullFn = func(ctx context.Context, image string) error {
	return exec.CommandContext(ctx, "docker", "pull", image).Run()
}

// Timeouts for the best-effort registry helpers: a hung ssh or pull must not
// stall the join forever.
var (
	registryCopyTimeout = 60 * time.Second
	registryPullTimeout = 2 * time.Minute
)

var joinCmd = &cobra.Command{
	Use:   "join",
	Short: "Join this host to the cluster's Docker Swarm",
	Long: `Joins this host to the cluster's Docker Swarm using a join token
obtained from an existing manager (docker swarm join-token worker|manager),
then initialises the local pmcluster state and starts the daemon.

The role (--role) selects the token type you need to pass: a worker token
joins as a worker, a manager token joins as a manager. After joining, pmcluster
verifies the node actually holds the requested role.

The daemon runs leader-aware: on the Swarm leader it serves; on every other
manager it stands by until Swarm elects it leader (failover). On a pure worker
node the daemon stays in standby.

Registry credentials (both optional, never fatal to the join):
  --copy-registry-creds <host>   ssh-copies the manager's ~/.docker/config.json
                                 onto this node (auths are MERGED into any
                                 existing config, which is backed up to
                                 config.json.bak) so private-registry image
                                 pulls stay fresh instead of going stale.
  --verify-registry-pull <image> runs a best-effort 'docker pull <image>' after
                                 joining to prove private pulls work.

Examples:
  pmcluster join --role worker  --token SWMTKN-1-... --manager 82.165.128.237:2377
  pmcluster join --role manager --token SWMTKN-1-... --manager 82.165.128.237:2377
  pmcluster join --role worker --token SWMTKN-1-... --manager 82.165.128.237:2377 \
    --copy-registry-creds 82.165.128.237 --verify-registry-pull ghcr.io/your-org/app:1

After a successful join the local state is initialised (data dir, config,
database migrations) and the systemd unit is installed + started.

Quorum note for managers: keep an ODD count. 2 managers are worse than 1
(either loss still breaks quorum); 3 managers survive any single failure.`,
	RunE: runJoin,
}

func init() {
	joinCmd.Flags().String("token", "", "swarm join token (from 'docker swarm join-token worker|manager' on a manager)")
	joinCmd.Flags().String("manager", "", "manager advertise address, e.g. 10.0.0.5:2377")
	joinCmd.Flags().String("role", "worker", "role to join as: worker or manager (must match the token type)")
	joinCmd.Flags().String("hostname", "", "hostname this node joins the Swarm under (default: current OS hostname)")
	joinCmd.Flags().String("copy-registry-creds", "", "ssh-host to copy the manager's ~/.docker/config.json from (e.g. 82.165.128.237) so private registry pulls stay fresh")
	joinCmd.Flags().String("verify-registry-pull", "", "after joining, best-effort 'docker pull <image>' to prove private registry credentials work (e.g. ghcr.io/your-org/app:1)")
	rootCmd.AddCommand(joinCmd)
}

func runJoin(cmd *cobra.Command, _ []string) error {
	token, _ := cmd.Flags().GetString("token")
	manager, _ := cmd.Flags().GetString("manager")
	role, _ := cmd.Flags().GetString("role")
	hostname, _ := cmd.Flags().GetString("hostname")
	copyCredsFrom, _ := cmd.Flags().GetString("copy-registry-creds")
	verifyPullImage, _ := cmd.Flags().GetString("verify-registry-pull")
	switch role {
	case "worker", "manager":
	default:
		return fmt.Errorf("--role must be 'worker' or 'manager', got %q", role)
	}
	if token == "" {
		return fmt.Errorf("join requires --token (run 'docker swarm join-token worker|manager' on a manager)")
	}
	if manager == "" {
		return fmt.Errorf("join requires --manager (the manager advertise address, e.g. 10.0.0.5:2377)")
	}

	// Set the OS hostname before joining: the Swarm records the hostname at
	// join time, and the leader-aware daemon matches os.Hostname() against the
	// node list — so pinning (`placement: <hostname>`) only works reliably
	// when the join-time hostname is the one you intend.
	if hostname != "" {
		if err := setNodeHostname(cmd.Context(), cmd.OutOrStdout(), hostname); err != nil {
			return err
		}
	}

	// Run docker swarm join via the docker CLI (transparent to the operator).
	joinArgs := []string{"swarm", "join", "--token", token, manager}
	jc := exec.CommandContext(cmd.Context(), "docker", joinArgs...)
	jc.Stdout = cmd.OutOrStdout()
	jc.Stderr = cmd.ErrOrStderr()
	fmt.Fprintf(cmd.OutOrStdout(), "→ docker %s\n", strings.Join(joinArgs, " "))
	if err := jc.Run(); err != nil {
		return fmt.Errorf("docker swarm join failed: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "✔ joined the Swarm.")

	// Verify the node actually holds the requested role (the token type decides;
	// catch token/--role mismatches early).
	if err := verifyJoinedRole(cmd.Context(), cmd.OutOrStdout(), role); err != nil {
		return err
	}

	// Initialise local pmcluster state (idempotent — an existing data dir is
	// reused, never wiped).
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := prepareJoinState(cmd.Context(), cmd.OutOrStdout(), cfg); err != nil {
		return err
	}

	// Registry auth gap: a node without credentials for the app registry
	// silently serves stale cached images (Docker never pulls when the image
	// exists locally, and a private registry 401s any pull attempt). Surface
	// the gap at join time instead of leaving it to bite a later deploy.
	verifyRegistryAuth(cmd.OutOrStdout())

	// Q2 — registry credentials on join: optionally bring the manager's
	// Docker config over and prove a private pull works. Both steps are
	// strictly best-effort: a registry problem must never fail the join
	// (verifyRegistryAuth above already warns when nothing is configured).
	if copyCredsFrom != "" {
		if copyRegistryCreds(cmd.Context(), cmd.OutOrStdout(), copyCredsFrom) {
			// Re-check now that the config was (re)written, so the operator
			// sees the credentials are actually in place.
			verifyRegistryAuth(cmd.OutOrStdout())
		}
	}
	if verifyPullImage != "" {
		verifyRegistryPull(cmd.Context(), cmd.OutOrStdout(), verifyPullImage)
	}

	// Install + start the daemon (systemd on Linux, hint otherwise).
	if err := ensureDaemonRunning(cmd.OutOrStdout()); err != nil {
		return err
	}

	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "✔ pmcluster join complete.")
	if role == "manager" {
		fmt.Fprintln(cmd.OutOrStdout(), "  This node is now a Swarm manager.")
		fmt.Fprintln(cmd.OutOrStdout(), "  The daemon is leader-aware: it serves when this node is the Swarm")
		fmt.Fprintln(cmd.OutOrStdout(), "  leader and stands by otherwise (failover).")
		fmt.Fprintln(cmd.OutOrStdout(), "  Quorum: keep the manager count ODD — 3 managers survive any single loss.")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "  This node is a Swarm worker.")
		fmt.Fprintln(cmd.OutOrStdout(), "  The daemon runs in standby on worker nodes. To serve here later,")
		fmt.Fprintln(cmd.OutOrStdout(), "  promote the node to manager:")
		fmt.Fprintln(cmd.OutOrStdout(), "    docker node promote <hostname>   # then it may be elected leader")
	}
	return nil
}

// setNodeHostname sets the OS hostname so the node joins the Swarm under the
// operator's chosen name (Swarm records the hostname at join time, and the
// leader-aware daemon matches os.Hostname() against the node list). It is a
// best-effort convenience: a failure to set the hostname is surfaced but is
// NOT fatal to the join itself — the node can join under its current name.
func setNodeHostname(ctx context.Context, out io.Writer, hostname string) error {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return nil
	}
	if !nodeNameRe.MatchString(hostname) {
		return fmt.Errorf("invalid hostname %q — letters, digits, dots, dashes and underscores only", hostname)
	}
	current, err := os.Hostname()
	if err == nil && current == hostname {
		fmt.Fprintf(out, "✔ hostname already %q.\n", hostname)
		return nil
	}

	// hostnamectl is the canonical Linux way; on other platforms (or when the
	// command is missing) surface a hint and continue — the join still works,
	// the node just keeps its current hostname.
	hc := exec.CommandContext(ctx, "hostnamectl", "set-hostname", hostname)
	if outB, err := hc.CombinedOutput(); err != nil {
		fmt.Fprintf(out, "⚠ could not set hostname to %q (%s) — joining under the current hostname.\n", hostname, strings.TrimSpace(string(outB)))
		return nil
	}
	fmt.Fprintf(out, "✔ hostname set to %q.\n", hostname)
	return nil
}

// verifyRegistryAuth warns when the local Docker config has no registry
// credentials. A node without credentials for a private app registry runs
// whatever image happens to be cached locally — silently stale — because
// Docker only pulls when the image is absent and private registries reject
// anonymous pulls. Best-effort: only a warning, the join itself succeeds.
func verifyRegistryAuth(out io.Writer) {
	cfgPath, err := dockerConfigPathFn()
	if err != nil {
		return
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintln(out, "⚠ no docker registry credentials found (no ~/.docker/config.json).")
		fmt.Fprintln(out, "  Private registries (e.g. ghcr.io/your-org) will silently fall back to stale")
		fmt.Fprintln(out, "  cached images. Authenticate before deploying private images:")
		fmt.Fprintln(out, "    docker login ghcr.io --username <user>")
		return
	}
	var cfg struct {
		Auths map[string]any `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || len(cfg.Auths) == 0 {
		fmt.Fprintln(out, "⚠ no docker registry credentials found (empty ~/.docker/config.json).")
		fmt.Fprintln(out, "  Private registries will silently fall back to stale cached images.")
		fmt.Fprintln(out, "    docker login ghcr.io --username <user>")
		return
	}
	fmt.Fprintln(out, "✔ docker registry credentials present (private pulls will stay fresh).")
}

// copyRegistryCreds copies the manager's ~/.docker/config.json onto this node
// with `ssh root@<host> 'cat /root/.docker/config.json'` and installs it as
// the local Docker config. When a local config already exists its auths are
// MERGED with the copied ones (copied entries win per-registry; every other
// local key such as credsStore survives) and the previous file is kept as
// config.json.bak, so nothing on the node is silently lost.
//
// Best-effort: every failure only warns and returns false — the join itself
// always succeeds.
func copyRegistryCreds(ctx context.Context, out io.Writer, host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	fmt.Fprintf(out, "→ copying registry credentials from %s (ssh root@%s:cat /root/.docker/config.json)\n", host, host)

	ctx, cancel := context.WithTimeout(ctx, registryCopyTimeout)
	defer cancel()
	remote, err := sshRemoteCatFn(ctx, host, "/root/.docker/config.json")
	if err != nil {
		fmt.Fprintf(out, "⚠ could not copy registry credentials from %s (%v) — joining without them.\n", host, err)
		return false
	}
	remote = bytes.TrimSpace(remote)
	if len(remote) == 0 {
		fmt.Fprintf(out, "⚠ %s has no /root/.docker/config.json (empty) — joining without credentials.\n", host)
		return false
	}
	if !json.Valid(remote) {
		fmt.Fprintf(out, "⚠ %s returned a non-JSON docker config — joining without credentials.\n", host)
		return false
	}

	cfgPath, err := dockerConfigPathFn()
	if err != nil {
		fmt.Fprintf(out, "⚠ could not resolve ~/.docker/config.json (%v) — joining without credentials.\n", err)
		return false
	}

	merged := remote
	if existing, err := os.ReadFile(cfgPath); err == nil && len(bytes.TrimSpace(existing)) > 0 {
		merged, err = mergeDockerConfigs(existing, remote)
		if err != nil {
			fmt.Fprintf(out, "⚠ could not merge registry credentials into the existing docker config (%v).\n", err)
			return false
		}
		// Keep the previous file around so the copy is reversible.
		bakPath := cfgPath + ".bak"
		if err := os.WriteFile(bakPath, existing, 0o600); err != nil {
			fmt.Fprintf(out, "⚠ could not back up the existing docker config to %s (%v).\n", bakPath, err)
		} else {
			fmt.Fprintf(out, "  previous ~/.docker/config.json backed up to %s\n", bakPath)
		}
	}

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		fmt.Fprintf(out, "⚠ could not create %s (%v) — joining without credentials.\n", filepath.Dir(cfgPath), err)
		return false
	}
	if err := os.WriteFile(cfgPath, merged, 0o600); err != nil {
		fmt.Fprintf(out, "⚠ could not write %s (%v) — joining without credentials.\n", cfgPath, err)
		return false
	}
	fmt.Fprintf(out, "✔ registry credentials copied from %s\n", host)
	return true
}

// mergeDockerConfigs folds the copied (remote) Docker config into the existing
// local one: the local file is the base — so its non-auth keys and any
// registry auths it already holds survive — and the remote fields overlay it,
// with the remote auths winning per-registry on collision. Unknown keys from
// both files (credsStore, HttpHeaders, ...) are preserved verbatim.
func mergeDockerConfigs(local, remote []byte) ([]byte, error) {
	decode := func(data []byte) (map[string]json.RawMessage, error) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return m, nil
	}
	remoteM, err := decode(remote)
	if err != nil {
		return nil, fmt.Errorf("invalid remote docker config: %w", err)
	}
	merged, err := decode(local)
	if err != nil {
		// An unreadable local config must not block the copy — the .bak still
		// holds the original bytes.
		merged = map[string]json.RawMessage{}
	}

	auths := map[string]json.RawMessage{}
	if raw, ok := merged["auths"]; ok {
		_ = json.Unmarshal(raw, &auths)
	}
	var remoteAuths map[string]json.RawMessage
	if raw, ok := remoteM["auths"]; ok {
		_ = json.Unmarshal(raw, &remoteAuths)
	}
	for host, cred := range remoteAuths {
		auths[host] = cred
	}
	for key, val := range remoteM {
		if key == "auths" {
			continue
		}
		merged[key] = val
	}
	encoded, err := json.Marshal(auths)
	if err != nil {
		return nil, err
	}
	merged["auths"] = encoded
	return json.MarshalIndent(merged, "", "  ")
}

// verifyRegistryPull proves private-registry pulls work on this node by
// running 'docker pull <image>' after any credential copy. Best-effort: a
// failure only warns ('registry pull failed — check credentials') — a pull
// problem must never fail the join.
func verifyRegistryPull(ctx context.Context, out io.Writer, image string) {
	image = strings.TrimSpace(image)
	if image == "" {
		return
	}
	fmt.Fprintf(out, "→ docker pull %s (verifying registry credentials)\n", image)
	ctx, cancel := context.WithTimeout(ctx, registryPullTimeout)
	defer cancel()
	if err := dockerPullFn(ctx, image); err != nil {
		fmt.Fprintln(out, "⚠ registry pull failed — check credentials.")
		fmt.Fprintf(out, "  docker pull %s: %v\n", image, err)
		return
	}
	fmt.Fprintln(out, "✔ registry pull succeeded (private image pulls will stay fresh).")
}

// verifyJoinedRole checks the node's actual Swarm role after joining and
// reports a mismatch against the requested --role.
func verifyJoinedRole(ctx context.Context, out io.Writer, want string) error {
	ic := exec.CommandContext(ctx, "docker", "info", "--format", "{{.Swarm.Control}}")
	outB, err := ic.Output()
	if err != nil {
		// Not fatal — docker info may be noisy on some setups; the daemon is
		// leader-aware regardless of role.
		fmt.Fprintln(out, "⚠ could not verify the joined role (docker info unavailable).")
		return nil
	}
	isManager := strings.TrimSpace(string(outB)) == "true"
	switch {
	case want == "manager" && !isManager:
		return fmt.Errorf("requested --role manager but this node joined as a WORKER — you passed a worker token; run 'docker swarm join-token manager' on a manager and re-join")
	case want == "worker" && isManager:
		fmt.Fprintln(out, "⚠ joined as a MANAGER despite --role worker (the token was a manager token).")
	}
	return nil
}

// prepareJoinState creates the data dir + default config + runs DB migrations
// when they are missing, mirroring what `pmcluster init` does WITHOUT creating
// a bootstrap user (join nodes are not the control-plane owner; on promotion
// the leader-aware restore brings the control plane over).
func prepareJoinState(ctx context.Context, out io.Writer, cfg *config.Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	// Every container volume on this platform is forced under the volume root
	// (/var/stack/data) with a bind mount, and backup agents write tarballs to
	// /var/stack/backup. Both must exist on the joined node BEFORE the Swarm
	// scheduler can place tasks here — otherwise a task that lands on this
	// node is rejected at volume-populate ("no such file or directory") and
	// the service update pauses. Mirrors ensureStorageDirs in cluster up.
	const volumeRoot = "/var/stack/data"
	const backupRoot = "/var/stack/backup"
	for _, dir := range []string{volumeRoot, backupRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create storage dir %s: %w", dir, err)
		}
	}
	fmt.Fprintln(out, "✔ volume-root storage directories ready (/var/stack/data, /var/stack/backup).")
	if err := writeDefaultConfig(cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if _, err := os.Stat(cfg.DBPath()); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat db path: %w", err)
		}
		st, err := store.Open(cfg.DBPath())
		if err != nil {
			return fmt.Errorf("open store (migrations): %w", err)
		}
		_ = st.Close()
		fmt.Fprintln(out, "✔ local control-plane database initialised (migrations applied).")
	} else {
		fmt.Fprintln(out, "✔ local control-plane database already present (reused).")
	}
	return nil
}
