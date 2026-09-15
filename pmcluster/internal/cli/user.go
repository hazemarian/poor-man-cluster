package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

var userCmd = &cobra.Command{
	Use:   "user",
	Short: "Manage pmcluster API users",
}

var userCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new API user and print a one-time bearer token",
	Long: `Generates a new bearer token, hashes it (argon2id), inserts the user row,
and prints the plaintext token ONCE on stdout. Save the token; pmcluster does
not store the plaintext.

Note: this writes directly to ~/.pmcluster/data.db. SQLite WAL mode handles
concurrent access with a running daemon, but if you suspect corruption you
can stop pmcluster (brew services stop pmcluster), run this, then restart.`,
	Args: cobra.ExactArgs(1),
	RunE: runUserCreate,
}

var userListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List API users (id, name, created) without revealing tokens",
	RunE:    runUserList,
}

func init() {
	userCmd.AddCommand(userCreateCmd, userListCmd)
	rootCmd.AddCommand(userCmd)
}

func runUserCreate(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return errors.New("name cannot be empty")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — run `pmcluster init` first", cfg.DataDir)
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	token, err := createUser(cmd.Context(), st, name)
	if err != nil {
		// Surface the user-already-exists case with a friendlier message.
		if errors.Is(err, store.ErrUserExists) {
			return fmt.Errorf("user %q already exists", name)
		}
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), `
✅ User %q created.

🔑 Bearer token (shown once — save it now):

   %s

   curl -H "Authorization: Bearer %s" http://%s/api/me
`, name, token, token, cfg.ListenAddr)
	return nil
}

// runUserList prints each API user's id/name/created without any token
// material (tokens are hashed at rest and never returned on read).
func runUserList(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — run `pmcluster init` first", cfg.DataDir)
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	users, err := st.ListUsers(cmd.Context())
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	if len(users) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no API users — use `pmcluster user create <name>`)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tCREATED")
	for _, u := range users {
		fmt.Fprintf(w, "%d\t%s\t%s\n", u.ID, u.Name, time.Unix(u.CreatedAt, 0).Format(time.RFC3339))
	}
	return w.Flush()
}
