package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialise pmcluster local state on this manager (one-time)",
	Long: `Creates ~/.pmcluster/, runs database migrations, and prints a one-time
bootstrap admin token used to authenticate against the pmcluster API.

Run this once on the manager host before pmcluster cluster up. Re-running on
an existing data directory is refused (use --force to wipe and start fresh —
DESTRUCTIVE).

The bootstrap token is shown ONCE on stdout. Save it; it cannot be recovered.
If lost, re-run with --force or create a second user with pmcluster user create.`,
	RunE: runInit,
}

func init() {
	initCmd.Flags().Bool("force", false, "wipe existing pmcluster state before initialising (DESTRUCTIVE)")
	initCmd.Flags().String("admin-name", "admin", "name for the bootstrap admin user")
}

func runInit(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	force, _ := cmd.Flags().GetBool("force")
	adminName, _ := cmd.Flags().GetString("admin-name")

	if err := prepareDataDir(cfg.DataDir, cfg.DBPath(), force); err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	count, err := st.CountUsers(cmd.Context())
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if count > 0 && !force {
		return fmt.Errorf("data dir already has %d user(s); use --force to wipe", count)
	}

	token, err := createUser(cmd.Context(), st, adminName)
	if err != nil {
		return err
	}

	if err := writeDefaultConfig(cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	printBootstrapToken(cmd.OutOrStdout(), cfg, adminName, token)
	return nil
}

// prepareDataDir creates the data directory (mode 0700). With --force,
// the DB and its WAL/SHM sidecars are removed first.
func prepareDataDir(dataDir, dbPath string, force bool) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if _, err := os.Stat(dbPath); err == nil {
		if !force {
			return fmt.Errorf("%s already exists; use --force to overwrite", dbPath)
		}
		for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove %s: %w", p, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat db path: %w", err)
	}
	return nil
}

// createUser returns the plaintext token; caller shows it once and
// discards.
func createUser(ctx context.Context, st *store.Store, name string) (string, error) {
	token, err := auth.GenerateToken()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	tokenID, secret := auth.SplitToken(token)
	hash, err := auth.HashToken(secret)
	if err != nil {
		return "", fmt.Errorf("hash token: %w", err)
	}
	if _, err := st.CreateUser(ctx, name, tokenID, hash); err != nil {
		return "", fmt.Errorf("insert user: %w", err)
	}
	return token, nil
}

// writeDefaultConfig is idempotent — operator edits win.
func writeDefaultConfig(cfg *config.Config) error {
	path := cfg.ConfigPath()
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body := fmt.Sprintf(`# pmcluster config — written by pmcluster init.
# Override any value via PMCLUSTER_<UPPER_FIELD> environment variables.

listen_addr: "%s"
log_level: "%s"
`, cfg.ListenAddr, cfg.LogLevel)
	return os.WriteFile(path, []byte(body), 0o600)
}

func printBootstrapToken(w io.Writer, cfg *config.Config, name, token string) {
	fmt.Fprintf(w, `
✅ pmcluster initialised.

   Data dir:    %s
   Config:      %s
   Listen:      %s

🔑 Bootstrap admin token (shown once — save it now):

   %s

   Use it as a Bearer token. Example:
   curl -H "Authorization: Bearer %s" http://%s/api/me

Next: pmcluster cluster up   (creates secrets/networks, deploys infra/observability/backup stacks)
`, cfg.DataDir, cfg.ConfigPath(), cfg.ListenAddr, token, token, cfg.ListenAddr)
}
