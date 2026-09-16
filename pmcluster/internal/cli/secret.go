package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
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

func init() {
	secretCreateCmd.Flags().String("scope", "service", "scope: cluster or service")
	secretCmd.AddCommand(secretCreateCmd, secretListCmd, secretShowCmd, secretVerifyCmd, secretDeleteCmd)
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

	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return fmt.Errorf("open encryption key: %w", err)
	}
	ct, err := cipher.Encrypt([]byte(value))
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	hash := secretHash(value)

	if _, err := st.CreateSecret(cmd.Context(), scope, name, ct, hash); err != nil {
		if errors.Is(err, store.ErrSecretExists) {
			return fmt.Errorf("secret %q already exists", name)
		}
		return fmt.Errorf("create secret: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), `
✅ Secret %q created (scope: %s).

🔑 Value (shown once — save it now):

   %s

   sha256: %s

Use it in a manifest:
   env:
     %s_VALUE: secrets(%s)
`, name, scope, value, hash, strings.ToUpper(strings.ReplaceAll(name, "-", "_")), name)
	return nil
}

func runSecretList(cmd *cobra.Command, _ []string) error {
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	secrets, err := st.ListSecrets(cmd.Context())
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
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	s, err := st.GetSecret(cmd.Context(), name)
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
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	s, err := st.GetSecret(cmd.Context(), name)
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
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.DeleteSecret(cmd.Context(), name); err != nil {
		if errors.Is(err, store.ErrSecretNotFound) {
			return fmt.Errorf("secret %q not found", name)
		}
		return fmt.Errorf("delete secret: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Secret %q deleted.\n", name)
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
