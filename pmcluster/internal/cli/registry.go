package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/registry"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Manage Docker registry credentials for private image pulls",
	Long: `Stores per-host credentials encrypted in SQLite AND runs 'docker login'
so they land in ~/.docker/config.json. 'docker stack deploy
--with-registry-auth' then forwards the manager's auth to every node so
workers can pull private images without their own login.`,
}

var registryAddCmd = &cobra.Command{
	Use:   "add <host>",
	Short: "Log in to a registry, store credentials encrypted",
	Long: `Examples:
  pmcluster registry add ghcr.io --username myuser --password-stdin    < /path/to/token
  pmcluster registry add docker.io --username myuser                   # prompts for password
  pmcluster registry add registry.example.com --username svc --password 'env-only-not-recommended'`,
	Args: cobra.ExactArgs(1),
	RunE: runRegistryAdd,
}

var registryListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured registries (without revealing passwords)",
	RunE:  runRegistryList,
}

var registryRemoveCmd = &cobra.Command{
	Use:   "remove <host>",
	Short: "Remove a registry from pmcluster's store and run docker logout",
	Args:  cobra.ExactArgs(1),
	RunE:  runRegistryRemove,
}

func init() {
	registryAddCmd.Flags().String("username", "", "registry username (required)")
	registryAddCmd.Flags().String("password", "", "registry password (avoid — leaks to shell history)")
	registryAddCmd.Flags().Bool("password-stdin", false, "read password from stdin (recommended for scripts)")

	registryCmd.AddCommand(registryAddCmd, registryListCmd, registryRemoveCmd)
	rootCmd.AddCommand(registryCmd)
}

func runRegistryAdd(cmd *cobra.Command, args []string) error {
	host := args[0]
	username, _ := cmd.Flags().GetString("username")
	if username == "" {
		return errors.New("--username is required")
	}

	password, err := readPassword(cmd)
	if err != nil {
		return err
	}
	if password == "" {
		return errors.New("password cannot be empty")
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

	svc := &registry.Service{
		Store:  st,
		Cipher: cipher,
		Runner: registry.ExecRunner{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()},
	}
	// Read the previous username first so the message can name it, the way
	// this command has always reported an update.
	previous, _ := st.GetRegistry(cmd.Context(), host)
	created, err := svc.Add(cmd.Context(), host, username, password)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(cmd.OutOrStdout(), "✅ Registry %s added (user: %s)\n", host, username)
	} else {
		was := username
		if previous != nil {
			was = previous.Username
		}
		fmt.Fprintf(cmd.OutOrStdout(), "✅ Registry %s updated (was %s, now %s)\n", host, was, username)
	}

	fmt.Fprintln(cmd.OutOrStdout(), "  Workers will receive these credentials on the next deploy via --with-registry-auth.")
	return nil
}

func runRegistryList(cmd *cobra.Command, _ []string) error {
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	regs, err := (&registry.Service{Store: st}).List(cmd.Context())
	if err != nil {
		return err
	}
	if len(regs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no registries configured — use `pmcluster registry add <host>`)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tUSERNAME\tCONFIGURED")
	for _, r := range regs {
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Host, r.Username,
			time.Unix(r.CreatedAt, 0).Format(time.RFC3339))
	}
	return w.Flush()
}

func runRegistryRemove(cmd *cobra.Command, args []string) error {
	host := args[0]
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	warning, err := (&registry.Service{Store: st, Runner: registry.ExecRunner{Err: cmd.ErrOrStderr()}}).Remove(cmd.Context(), host)
	if err != nil {
		if errors.Is(err, store.ErrRegistryNotFound) {
			return fmt.Errorf("registry %q not configured", host)
		}
		return err
	}
	if warning != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✅ Registry %q removed.\n", host)
	return nil
}

// readPassword resolves --password, --password-stdin, or an interactive prompt.
func readPassword(cmd *cobra.Command) (string, error) {
	pwFlag, _ := cmd.Flags().GetString("password")
	stdinFlag, _ := cmd.Flags().GetBool("password-stdin")

	if pwFlag != "" && stdinFlag {
		return "", errors.New("--password and --password-stdin are mutually exclusive")
	}
	if pwFlag != "" {
		return pwFlag, nil
	}
	if stdinFlag {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}

	fmt.Fprint(cmd.ErrOrStderr(), "Password: ")
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
