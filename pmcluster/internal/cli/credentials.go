package cli

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

var credsCmd = &cobra.Command{
	Use:   "credentials",
	Short: "Inspect bootstrap credentials managed by pmcluster",
	Long: `pmcluster generates random passwords for the bundled components on
first 'cluster up' and stores them encrypted in SQLite. These commands
retrieve them after the fact.`,
}

var credsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all managed credentials (without revealing passwords)",
	RunE:  runCredsList,
}

var credsShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Decrypt and print one credential's username + password",
	Long: `Decrypts via ~/.pmcluster/.encryption_key and prints to stdout. Treat
the output as sensitive.

Names: traefik_dashboard | portainer | openobserve_admin`,
	Args: cobra.ExactArgs(1),
	RunE: runCredsShow,
}

var credsRotateCmd = &cobra.Command{
	Use:   "rotate <name>",
	Short: "Generate a new password, swap the Swarm secret, restart the consuming service",
	Long: `Rotates a managed credential end-to-end (generate → re-encrypt →
SecretRemove → SecretCreate → ForceUpdateService).

If the consuming service is still running with the secret mounted,
SecretRemove fails ("secret in use"). Scale the service to 0
(docker service scale <name>=0), rotate, then scale back.

Names: traefik_dashboard | portainer | openobserve_admin`,
	Args: cobra.ExactArgs(1),
	RunE: runCredsRotate,
}

func init() {
	credsCmd.AddCommand(credsListCmd, credsShowCmd, credsRotateCmd)
	rootCmd.AddCommand(credsCmd)
}

func runCredsList(cmd *cobra.Command, _ []string) error {
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	creds, err := st.ListCredentials(cmd.Context())
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}
	if len(creds) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no managed credentials yet — run `pmcluster cluster up`)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tUSERNAME\tSWARM SECRET\tCREATED\tROTATED")
	for _, c := range creds {
		rotated := "—"
		if c.RotatedAt.Valid {
			rotated = time.Unix(c.RotatedAt.Int64, 0).Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Name, c.Kind, c.Username, c.SwarmSecretName,
			time.Unix(c.CreatedAt, 0).Format(time.RFC3339), rotated,
		)
	}
	return w.Flush()
}

func runCredsShow(cmd *cobra.Command, args []string) error {
	name := args[0]
	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	c, err := st.GetCredential(cmd.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrCredentialNotFound) {
			return fmt.Errorf("credential %q not found (try `pmcluster credentials list`)", name)
		}
		return fmt.Errorf("get credential: %w", err)
	}

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return fmt.Errorf("open encryption key: %w", err)
	}
	plain, err := cipher.Decrypt(c.PasswordCiphertext)
	if err != nil {
		return fmt.Errorf("decrypt: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"name:           %s\nkind:           %s\nusername:       %s\nswarm secret:   %s\npassword:       %s\n",
		c.Name, c.Kind, c.Username, c.SwarmSecretName, string(plain))
	return nil
}

func runCredsRotate(cmd *cobra.Command, args []string) error {
	name := args[0]
	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return fmt.Errorf("open encryption key: %w", err)
	}
	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dc.Close() }()

	mgr := &cluster.CredentialsManager{
		Store:    st,
		Cipher:   cipher,
		Docker:   dc,
		Deployer: cluster.NewDockerCLIDeployer(cmd.OutOrStdout()),
	}
	rotated, err := mgr.Rotate(cmd.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrCredentialNotFound) {
			return fmt.Errorf("credential %q not found", name)
		}
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), `
✅ Credential %q rotated.

🔑 New password (shown once — save it now):

   %s

   user:  %s
   secret: %s
`, rotated.Name, rotated.Password, rotated.Username, rotated.SwarmSecretName)
	return nil
}

// openStore loads config, refuses if the data dir isn't initialised, and
// opens the store. Returns config too — callers need EncryptionKeyPath().
func openStore() (*store.Store, *config.Config, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("data directory not initialised at %s — run `pmcluster init` first", cfg.DataDir)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	return st, cfg, nil
}
