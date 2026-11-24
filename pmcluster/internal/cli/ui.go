package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	uistore "github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// uiCmd is the break-glass path for console identity.
//
// SSO says WHO may reach the console; the console's own table says what they may
// do. That table lives in the console's SQLite file (the pmui-data volume), which
// an operator with root on the node can reach — so when every identity is pending
// and nobody can approve anybody from inside the console, this is how the deadlock
// breaks. It writes the same rows the console reads.
var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "Inspect and approve console identities (break-glass)",
	Long: `Operates on the operator console's own user table.

With SSO, oauth2-proxy asserts identity through headers and this table decides the
role: an unknown identity is created in the pending state, sees only a waiting
page, and becomes usable when an admin approves it and assigns a role. If every
identity is pending — a fresh cluster, or a console whose only admin left — use
'pmcluster ui approve' here, which writes the row directly.

The default data dir is the console's volume as docker presents it on the node.`,
}

var uiUsersCmd = &cobra.Command{
	Use:     "users",
	Aliases: []string{"list"},
	Short:   "List console identities (role, status, source)",
	RunE:    runUIUsers,
}

var uiPendingCmd = &cobra.Command{
	Use:   "pending",
	Short: "List identities waiting for approval",
	RunE:  runUIPending,
}

var uiApproveCmd = &cobra.Command{
	Use:   "approve <email>",
	Short: "Approve a pending identity and give it a role",
	Args:  cobra.ExactArgs(1),
	RunE:  runUIApprove,
}

var uiRejectCmd = &cobra.Command{
	Use:   "reject <email>",
	Short: "Delete a pending identity so the next login starts over",
	Args:  cobra.ExactArgs(1),
	RunE:  runUIReject,
}

func init() {
	uiCmd.PersistentFlags().String("data-dir", "/var/lib/docker/volumes/edge_pmui-data/_data", "the console's data directory (where pmcluster-ui.db lives)")
	uiApproveCmd.Flags().String("role", uistore.RoleViewer, "role to grant: admin, operator or viewer")
	uiCmd.AddCommand(uiUsersCmd, uiPendingCmd, uiApproveCmd, uiRejectCmd)
	rootCmd.AddCommand(uiCmd)
}

func openUIStore(cmd *cobra.Command) (*uistore.Store, error) {
	dir, _ := cmd.Flags().GetString("data-dir")
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("--data-dir is required")
	}
	// Refuse to CREATE the database. The console runs as an unprivileged user
	// inside its container; a root-run CLI that creates the file first leaves it
	// owned by root, and every write the console then makes fails — silently, as
	// a redirect. That is exactly what happened on 2026-11-24.
	dbPath := filepath.Join(dir, "pmcluster-ui.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("no console database at %s — start the console once (the edge service) so it creates it, then retry", dbPath)
	}
	return uistore.Open(dir)
}

func runUIUsers(cmd *cobra.Command, _ []string) error {
	st, err := openUIStore(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	users, err := st.ListUsers(cmd.Context())
	if err != nil {
		return err
	}
	if len(users) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no console identities — the first login creates one in pending)")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tEMAIL\tROLE\tSTATUS\tSOURCE")
	for _, u := range users {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", u.ID, u.Username, dash(u.Email), u.Role, u.Status, u.AuthSource)
	}
	return w.Flush()
}

func runUIPending(cmd *cobra.Command, _ []string) error {
	st, err := openUIStore(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	pending, err := st.ListPending(cmd.Context())
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no identities are waiting for approval")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tEMAIL\tREQUESTED")
	for _, u := range pending {
		fmt.Fprintf(w, "%d\t%s\t%s\n", u.ID, dash(u.Email), u.Username)
	}
	return w.Flush()
}

func runUIApprove(cmd *cobra.Command, args []string) error {
	role, _ := cmd.Flags().GetString("role")
	if !validUIRole(role) {
		return fmt.Errorf("role must be one of admin, operator, viewer (got %q)", role)
	}
	st, err := openUIStore(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	email := strings.TrimSpace(strings.ToLower(args[0]))
	u, err := st.ByEmail(cmd.Context(), email)
	if err != nil {
		return fmt.Errorf("no console identity with email %q (see `pmcluster ui pending`)", email)
	}
	if err := st.SetStatus(cmd.Context(), u.ID, uistore.StatusActive, role); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "approved %s as %s\n", email, role)
	return nil
}

func runUIReject(cmd *cobra.Command, args []string) error {
	st, err := openUIStore(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	email := strings.TrimSpace(strings.ToLower(args[0]))
	u, err := st.ByEmail(cmd.Context(), email)
	if err != nil {
		return fmt.Errorf("no console identity with email %q", email)
	}
	if u.Status != uistore.StatusPending {
		return fmt.Errorf("%s is %s, not pending — refusing to delete an account that in use", email, u.Status)
	}
	if err := st.DeleteUser(cmd.Context(), u.ID); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "removed pending identity %s\n", email)
	return nil
}

func validUIRole(role string) bool {
	switch role {
	case uistore.RoleAdmin, uistore.RoleOperator, uistore.RoleViewer:
		return true
	}
	return false
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
