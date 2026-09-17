package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/apikeys"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
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
can stop pmcluster (e.g. brew services stop pmcluster on macOS), run this,
then restart.`,
	Args: cobra.ExactArgs(1),
	RunE: runUserCreate,
}

var userListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List API users (id, name, created) without revealing tokens",
	RunE:    runUserList,
}

var userRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove an API user and revoke its bearer token immediately",
	Long: `Deletes the user row so its bearer token stops working on the next
request. Refuses to remove the "edge" user (the operator console's daemon
token) or the user you are currently authenticated as (CLI writes directly
to the store; the guard is enforced by name).`,
	Args: cobra.ExactArgs(1),
	RunE: runUserRemove,
}

func init() {
	userCmd.AddCommand(userCreateCmd, userListCmd, userRemoveCmd)
	rootCmd.AddCommand(userCmd)
}

func runUserCreate(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return errors.New("name cannot be empty")
	}

	svc, closeFn, err := backendAPIKeys(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	_, token, err := svc.Create(cmd.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrUserExists) {
			return fmt.Errorf("user %q already exists", name)
		}
		return err
	}

	base := ""
	if rc := remoteClient(cmd); rc != nil {
		base = apiURL
	} else if cfg := loadConfig(cmd); cfg != nil {
		base = "http://" + cfg.ListenAddr
	}
	fmt.Fprintf(cmd.OutOrStdout(), `
✅ User %q created.

🔑 Bearer token (shown once — save it now):

   %s

   curl -H "Authorization: Bearer %s" %s/api/me
`, name, token, token, base)
	return nil
}

// runUserList prints each API user's id/name/created without any token
// material (tokens are hashed at rest and never returned on read).
func runUserList(cmd *cobra.Command, _ []string) error {
	svc, closeFn, err := backendAPIKeys(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	users, err := svc.List(cmd.Context())
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	return printUsers(cmd, users)
}

func printUsers(cmd *cobra.Command, users []apikeys.APIKey) error {
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

// runUserRemove deletes a user by name. The edge-user + self-delete guards
// live in the APIKeys service (shared with the daemon API).
func runUserRemove(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return errors.New("name cannot be empty")
	}

	svc, closeFn, err := backendAPIKeys(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	users, err := svc.List(cmd.Context())
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	var id int64
	for _, u := range users {
		if u.Name == name {
			id = u.ID
			break
		}
	}
	if id == 0 {
		return fmt.Errorf("user %q not found", name)
	}

	if err := svc.Delete(cmd.Context(), id); err != nil {
		if errors.Is(err, service.ErrEdgeUserProtected) {
			return fmt.Errorf("the %q user is required by the operator console and cannot be removed", name)
		}
		if errors.Is(err, store.ErrUserNotFound) {
			return fmt.Errorf("user %q not found", name)
		}
		return fmt.Errorf("delete user: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ User %q removed; its bearer token is revoked.\n", name)
	return nil
}
