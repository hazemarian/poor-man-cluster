package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/secrets"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage DB-backed secrets (AES-GCM encrypted, shown as hashes)",
	Long: `Secrets live in the pmcluster database, encrypted with AES-256-GCM
using ~/.pmcluster/.encryption_key. The plaintext is shown ONCE at creation
(like webhook secrets); afterwards only the sha256 hash is displayed, so a
value can be verified without ever being revealed.

DSL usage — env value injection:
  env:
    ADMIN_PASS: secrets(app_secret)   # value = /run/secrets/app_secret
                                       # (also mounts the file when using
                                       #  the secrets: array in the service)

Secrets referenced by a deployed stack can't be deleted until the stack
stops referencing them.`,
}

var secretCreateCmd = &cobra.Command{
	Use:   "create <name> [value]",
	Short: "Create a new secret (value shown once; prompt or stdin if omitted)",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runSecretCreate,
}

var secretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List secrets (name, scope, sha256 hash, created) — never plaintext",
	RunE:  runSecretList,
}

var secretShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show a secret's hash and metadata (never plaintext)",
	Args:  cobra.ExactArgs(1),
	RunE:  runSecretShow,
}

var secretVerifyCmd = &cobra.Command{
	Use:   "verify <name> <value>",
	Short: "Compare a supplied value against the stored hash",
	Args:  cobra.ExactArgs(2),
	RunE:  runSecretVerify,
}

var secretDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a secret (refused if referenced by a deployed stack)",
	Args:  cobra.ExactArgs(1),
	RunE:  runSecretDelete,
}

var secretEditCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Update a secret's value (also mirrored to the Docker Swarm)",
	Args:  cobra.ExactArgs(1),
	RunE:  runSecretEdit,
}

var secretHealCmd = &cobra.Command{
	Use:   "heal",
	Short: "Verify every secret decrypts with the current key and materialize its swarm mirror",
	Long: `Walks every stored secret and makes it deployable again:

  1. decrypt the row with the current encryption key — a row that does not
     decrypt was sealed by an older ~/.pmcluster/.encryption_key and must be
     re-supplied (AES-GCM cannot be reversed without the key that sealed it);
  2. repair a stale hash (translation derives the Docker swarm secret name
     from the stored hash, so a mismatch references a missing object);
  3. create the content-addressed Docker swarm secret when it is missing.

Values are never changed by a heal pass. To recover a row that cannot be
decrypted, read the plaintext from a container that still mounts it
(docker exec <container> cat /run/secrets/<name>) and re-supply it:

   printf '%s' '<value>' | pmcluster secret edit <name>

Managed credentials (traefik_dashboard, openobserve_admin, …) are healed by
'pmcluster cluster update', not by this command.`,
	Args: cobra.NoArgs,
	RunE: runSecretHeal,
}

func init() {
	secretCreateCmd.Flags().String("scope", "service", "scope: cluster or service")
	secretCreateCmd.Flags().String("stack", "", "stack this secret belongs to (service scope only)")
	secretEditCmd.Flags().String("value", "", "new secret value (or pipe via stdin)")
	secretHealCmd.Flags().Bool("dry-run", false, "report what would change without writing or creating swarm objects")
	secretCmd.AddCommand(secretCreateCmd, secretListCmd, secretShowCmd, secretVerifyCmd, secretEditCmd, secretDeleteCmd, secretHealCmd)
	rootCmd.AddCommand(secretCmd)
}

// readSecretValue resolves the value for `secret create`: positional arg
// wins; otherwise try stdin (pipe); otherwise prompt with terminal echo off.
func readSecretValue(cmd *cobra.Command, arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return "", errors.New("no value given — pass it as the second argument or pipe it via stdin (secret values are never echoed interactively)")
}

func runSecretCreate(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return errors.New("name: required")
	}
	value := ""
	if len(args) > 1 {
		value = args[1]
	}
	value, err := readSecretValue(cmd, value)
	if err != nil {
		return err
	}
	scope, _ := cmd.Flags().GetString("scope")
	switch scope {
	case "cluster", "service":
	default:
		return fmt.Errorf("scope must be cluster or service, got %q", scope)
	}
	stack, _ := cmd.Flags().GetString("stack")

	svc, closeFn, err := backendSecrets(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	hash := secretHash(value)

	if _, err := svc.Create(cmd.Context(), scope, stack, name, value); err != nil {
		if errors.Is(err, store.ErrSecretExists) {
			return fmt.Errorf("secret %q already exists", name)
		}
		return fmt.Errorf("create secret: %w", err)
	}

	// The DSL renders secrets(name) as external:true — the container mounts
	// a Docker SWARM secret, not the encrypted DB row. So the DB row alone is
	// not enough: the same value must also exist as a swarm secret, otherwise
	// the stack deploy fails with 'secret not found: <name>'. Mirror it into
	// Docker when running on a node with daemon access (local mode). The DB
	// row remains the encrypted source of truth for rotations/CLI display.
	swarmMirrored := mirrorSwarmSecret(cmd, name, value, hash)

	extra := ""
	if swarmMirrored {
		extra = " (+ mirrored to the Docker Swarm so containers can mount it)"
	}
	fmt.Fprintf(cmd.OutOrStdout(), `
✅ Secret %q created (scope: %s)%s.

🔑 Value (shown once — save it now):

   %s

   sha256: %s

Use it in a manifest:
   env:
     %s_VALUE: secrets(%s)
`, name, scope, extra, value, hash, strings.ToUpper(strings.ReplaceAll(name, "-", "_")), name)
	return nil
}

// swarmSecretMirrored ensures the content-addressed swarm secret backing a
// value exists, creating it when missing. Names are CONTENT-ADDRESSED
// (<name>_<sha8-of-value> — store.SwarmSecretName), so the same value always
// maps to the same object and the immutable in-use original is never mutated;
// a real value change simply mints a new name. Reports whether it created a
// new object.
func swarmSecretMirrored(ctx context.Context, dc runtime.Client, name, value, hash string) (bool, error) {
	target := store.SwarmSecretName(name, hash)
	exists, err := dc.SecretExists(ctx, target)
	if err != nil {
		return false, fmt.Errorf("check swarm secret %s: %w", target, err)
	}
	if exists {
		return false, nil
	}
	if err := dc.SecretCreate(ctx, runtime.SecretSpec{
		Name:   target,
		Data:   []byte(value),
		Labels: map[string]string{"pmcluster.secret": "true", "pmcluster.secret.hash": hash},
	}); err != nil {
		return false, fmt.Errorf("create swarm secret %s: %w", target, err)
	}
	return true, nil
}

// mirrorSwarmSecret makes the given value available to Docker Swarm so
// `docker stack deploy` can mount it (DSL secrets(name) renders
// external:true), using the content-addressed name translation derives from
// the DB hash. Best-effort: warnings to stderr, never fails the command.
func mirrorSwarmSecret(cmd *cobra.Command, name, value string, hash string) bool {
	if rc := remoteClient(cmd); rc != nil {
		return false // remote mode has no local daemon access
	}
	dc, derr := dockerNewFn()
	if derr != nil {
		// No daemon reachable — the operator may be on a user machine
		// creating secrets to use on the cluster later. Warn, don't fail:
		// the deploy on the manager would surface the missing secret.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"   ⚠ docker daemon unreachable — swarm secret NOT mirrored (%v).\n"+
				"     Deploying a stack that references %q will fail until you run:\n"+
				"       printf '%s' | docker secret create %s -\n",
			derr, name, value, name)
		return false
	}
	defer func() { _ = dc.Close() }()
	if _, err := swarmSecretMirrored(cmd.Context(), dc, name, value, hash); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"   ⚠ could not mirror the swarm secret for %q (%v) — mirror skipped.\n", name, err)
		return false
	}
	return true
}

func runSecretList(cmd *cobra.Command, _ []string) error {
	svc, closeFn, err := backendSecrets(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	secrets, err := svc.List(cmd.Context(), "", "")
	if err != nil {
		return fmt.Errorf("list secrets: %w", err)
	}
	if len(secrets) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no secrets yet — run `pmcluster secret create <name> <value>`)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCOPE\tSHA256\tCREATED")
	for _, s := range secrets {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			s.Name, s.Scope, shortHash(s.Hash), time.Unix(s.CreatedAt, 0).Format(time.RFC3339))
	}
	return w.Flush()
}

func runSecretShow(cmd *cobra.Command, args []string) error {
	name := args[0]
	svc, closeFn, err := backendSecrets(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	s, err := svc.Get(cmd.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrSecretNotFound) {
			return fmt.Errorf("secret %q not found", name)
		}
		return fmt.Errorf("get secret: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"name:     %s\nscope:    %s\nsha256:   %s\ncreated:  %s\n",
		s.Name, s.Scope, s.Hash, time.Unix(s.CreatedAt, 0).Format(time.RFC3339))
	return nil
}

func runSecretVerify(cmd *cobra.Command, args []string) error {
	name, value := args[0], args[1]
	svc, closeFn, err := backendSecrets(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	s, err := svc.Get(cmd.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrSecretNotFound) {
			return fmt.Errorf("secret %q not found", name)
		}
		return fmt.Errorf("get secret: %w", err)
	}

	if s.Hash == secretHash(value) {
		fmt.Fprintln(cmd.OutOrStdout(), "✓ hash matches")
		return nil
	}
	return errors.New("✗ hash does not match")
}

func runSecretDelete(cmd *cobra.Command, args []string) error {
	name := args[0]
	svc, closeFn, err := backendSecrets(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := svc.Delete(cmd.Context(), name); err != nil {
		if errors.Is(err, store.ErrSecretNotFound) {
			return fmt.Errorf("secret %q not found", name)
		}
		return fmt.Errorf("delete secret: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Secret %q deleted.\n", name)
	return nil
}

func runSecretEdit(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return errors.New("name: required")
	}
	value, _ := cmd.Flags().GetString("value")
	value, err := readSecretValue(cmd, value)
	if err != nil {
		return err
	}

	svc, closeFn, err := backendSecrets(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := svc.Update(cmd.Context(), name, value); err != nil {
		if errors.Is(err, store.ErrSecretNotFound) {
			return fmt.Errorf("secret %q not found", name)
		}
		return fmt.Errorf("update secret: %w", err)
	}

	// Keep the Docker Swarm mirror in sync. The new value is content-addressed
	// into <name>_<sha8-of-value> (store.SwarmSecretName) — the in-use
	// original is immutable and cannot be replaced; a new value simply mints
	// a new name, and the DB hash is how translation finds it.
	hash := secretHash(value)
	swarmMirrored := mirrorSwarmSecret(cmd, name, value, hash)

	extra := ""
	if swarmMirrored {
		extra = fmt.Sprintf(" (+ mirrored to the Docker Swarm as %q so containers mount the new value on next deploy)", store.SwarmSecretName(name, hash))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Secret %q updated (sha256: %s)%s.\n",
		name, hash, extra)
	return nil
}

// secretHash is the sha256 hex fingerprint of a plaintext secret value.
func secretHash(v string) string { return store.SecretHash(v) }

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// runSecretHeal verifies every secret row against the current encryption key
// and materializes the content-addressed swarm mirror for each recoverable
// value. It exits non-zero when a row still needs operator attention (an
// unrecoverable ciphertext, or a swarm mirror that could not be created).
func runSecretHeal(cmd *cobra.Command, _ []string) error {
	if rc := remoteClient(cmd); rc != nil {
		return errors.New("secret heal must run on a node that holds the store and ~/.pmcluster/.encryption_key (not in remote mode)")
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return fmt.Errorf("open encryption key: %w", err)
	}

	opts := secrets.HealOptions{DryRun: dryRun}
	if dc, derr := dockerNewFn(); derr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"   ⚠ docker daemon unreachable (%v) — swarm mirrors were not checked.\n"+
				"     Run this again on a cluster node to materialize missing swarm secrets.\n", derr)
	} else {
		defer func() { _ = dc.Close() }()
		opts.Mirror = func(ctx context.Context, name, value, hash string) (bool, error) {
			return swarmSecretMirrored(ctx, dc, name, value, hash)
		}
	}

	rep, err := secrets.NewLocal(st, cipher).Heal(cmd.Context(), opts)
	if err != nil {
		return fmt.Errorf("heal secrets: %w", err)
	}

	out := cmd.OutOrStdout()
	if len(rep.Entries) == 0 {
		fmt.Fprintln(out, "(no secrets yet — nothing to heal)")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tNAME\tSCOPE\tSHA256\tDETAIL")
	for _, e := range rep.Entries {
		hash := "—"
		if e.Hash != "" {
			hash = shortHash(e.Hash)
		}
		scope := e.Scope
		if e.Stack != "" {
			scope += "/" + e.Stack
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Status, e.Name, scope, hash, strings.Join(e.Actions, "; "))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	mode := ""
	if dryRun {
		mode = " (dry-run — nothing was changed)"
	}
	fmt.Fprintf(out, "\n%d secret(s) checked: %d ok, %d repaired, %d degraded, %d unrecoverable%s\n",
		len(rep.Entries), rep.OK, rep.Repaired, rep.Degraded, rep.Unrecoverable, mode)

	if rep.Unrecoverable > 0 {
		return fmt.Errorf("%d secret(s) cannot be decrypted with the current key — re-supply the value with `pmcluster secret edit` (see DETAIL above)", rep.Unrecoverable)
	}
	if rep.Degraded > 0 {
		return fmt.Errorf("%d secret(s) could not be mirrored into the swarm — re-run on a cluster node", rep.Degraded)
	}
	return nil
}
