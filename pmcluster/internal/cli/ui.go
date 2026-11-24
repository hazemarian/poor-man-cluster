package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	uistore "github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// uiCmd is the break-glass path for console identity.
//
// SSO says WHO may reach the console; the console's own table says what they may
// do. That table lives in the console's named volume (pmui-data), and this command
// reaches it the same way the console does — by mounting the VOLUME into a
// throwaway container and running as the console's own uid.
//
// It never opens /var/lib/docker/volumes/... on the host. Doing that as root is how
// a root-owned database appeared that the console (uid 10001) could not write, which
// then looked like a redirect bug rather than a permissions one.
var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "Inspect and approve console identities (break-glass)",
	Long: `Operates on the operator console's own user table.

With SSO, oauth2-proxy asserts identity through headers and this table decides the
role: an unknown identity is created in the pending state, sees only a waiting
page, and becomes usable when an admin approves it and assigns a role. If every
identity is pending — a fresh cluster, or a console whose only admin left — use
'pmcluster ui approve' here.

The table lives in the console's volume, so this runs inside a throwaway container
that mounts that volume as the console's uid: ownership stays with the console and
no host path is touched. Pass --data-dir to read a directory directly instead.`,
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

var uiGrantCmd = &cobra.Command{
	Use:   "grant <email>",
	Short: "Create-or-activate a console identity with a role (bootstrap)",
	Args:  cobra.ExactArgs(1),
	Long: `Grants a role to an SSO identity, creating the row if the person has never
signed in.

'approve' answers a request that exists; 'grant' is for the case where it does not
yet — a fresh console, or an operator who wants their own account ready before the
first login. The identity is still only reachable through the SSO proxy: this row
says what it may do, not that GitHub will authenticate it.`,
	RunE: runUIGrant,
}

var uiDisableCmd = &cobra.Command{
	Use:   "disable <email>",
	Short: "Keep an account but stop it signing in (offboarding)",
	Args:  cobra.ExactArgs(1),
	RunE:  runUIDisable,
}

var uiDeleteCmd = &cobra.Command{
	Use:     "delete <email>",
	Aliases: []string{"remove", "rm"},
	Short:   "Delete a console identity outright",
	Args:    cobra.ExactArgs(1),
	RunE:    runUIDelete,
}

func init() {
	uiCmd.PersistentFlags().String("volume", "edge_pmui-data", "the console's docker volume; empty means read --data-dir directly")
	uiCmd.PersistentFlags().String("data-dir", "", "read this directory directly (only when the console is not a swarm service)")
	uiCmd.PersistentFlags().String("uid", "", "uid to run as inside the volume container (default: the console's own)")
	uiCmd.PersistentFlags().String("image", "alpine:3.20", "image used to mount the volume")
	uiCmd.PersistentFlags().Bool("in-volume", false, "internal: already running inside the volume container")
	uiApproveCmd.Flags().String("role", uistore.RoleViewer, "role to grant: admin, operator or viewer")
	uiGrantCmd.Flags().String("role", uistore.RoleViewer, "role to grant: admin, operator or viewer")
	uiCmd.AddCommand(uiUsersCmd, uiPendingCmd, uiApproveCmd, uiRejectCmd, uiGrantCmd, uiDisableCmd, uiDeleteCmd)
	rootCmd.AddCommand(uiCmd)
}

// inVolume runs this command inside a container that mounts the console's volume,
// and reports whether it handled the call. The container runs as the console's uid
// so the file it writes stays writable by the console.
func inVolume(cmd *cobra.Command) (bool, error) {
	flags := cmd.Flags()
	if already, _ := flags.GetBool("in-volume"); already {
		return false, nil
	}
	vol, _ := flags.GetString("volume")
	vol = strings.TrimSpace(vol)
	if vol == "" {
		return false, nil // explicit --data-dir mode
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return false, fmt.Errorf("docker is required to reach the console volume %q (or pass --data-dir)", vol)
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	image, _ := flags.GetString("image")
	spec := containerArgs(vol, consoleUID(cmd), exe, image, forwardArgs(os.Args[1:]))
	run := exec.Command("docker", spec...) //nolint:gosec // argv built here, never from a user shell string
	run.Stdout, run.Stderr, run.Stdin = cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin()
	if err := run.Run(); err != nil {
		return true, fmt.Errorf("run in volume %s: %w", vol, err)
	}
	return true, nil
}

// containerArgs is the docker argv: the volume at /data, this binary read-only in
// the container, and the same command re-issued with --data-dir /data, the volume
// cleared (no recursion) and --in-volume set.
func containerArgs(vol, uid, exe, image string, forward []string) []string {
	args := []string{
		"run", "--rm",
		"-u", uid,
		"-v", vol + ":/data",
		"-v", exe + ":/usr/local/bin/pmcluster:ro",
		"--entrypoint", "/usr/local/bin/pmcluster",
		image,
	}
	args = append(args, forward...)
	args = append(args, "--data-dir", "/data", "--volume", "", "--in-volume")
	return args
}

// forwardArgs keeps the operator's command line (ui approve x@y --role admin) and
// drops this command's own plumbing, which the container replaces.
func forwardArgs(argv []string) []string {
	drop := map[string]bool{"--volume": true, "--data-dir": true, "--uid": true, "--image": true, "--in-volume": true}
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if drop[a] {
			if a != "--in-volume" {
				i++ // skip its value
			}
			continue
		}
		if name, _, ok := strings.Cut(a, "="); ok && drop[name] {
			continue
		}
		out = append(out, a)
	}
	return out
}

// consoleUID asks the running console which uid it uses, so the file it owns stays
// writable by it. Falls back to the image's conventional uid.
func consoleUID(cmd *cobra.Command) string {
	if v, _ := cmd.Flags().GetString("uid"); strings.TrimSpace(v) != "" {
		return v
	}
	out, err := exec.Command("docker", "ps", "-q", "-f", "name=edge_pmcluster-edge").Output()
	if err == nil {
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			if u, uerr := exec.Command("docker", "exec", ids[0], "id", "-u").Output(); uerr == nil { //nolint:gosec // id from docker ps
				if s := strings.TrimSpace(string(u)); s != "" {
					return s
				}
			}
		}
	}
	return "10001"
}

func openUIStore(cmd *cobra.Command) (*uistore.Store, error) {
	dir, _ := cmd.Flags().GetString("data-dir")
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("no data dir — run on a node with the console volume, or pass --data-dir")
	}
	// Never CREATE the database: the console runs as an unprivileged user, and a
	// root-created file it cannot write is worse than a clear error.
	dbPath := filepath.Join(dir, "pmcluster-ui.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("no console database at %s — start the console once (the edge service) so it creates it, then retry", dbPath)
	}
	return uistore.Open(dir)
}

func runUIUsers(cmd *cobra.Command, _ []string) error {
	if done, err := inVolume(cmd); done {
		return err
	}
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
	if done, err := inVolume(cmd); done {
		return err
	}
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
	if done, err := inVolume(cmd); done {
		return err
	}
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
	if done, err := inVolume(cmd); done {
		return err
	}
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
		return fmt.Errorf("%s is %s, not pending — refusing to delete an account in use", email, u.Status)
	}
	if err := st.DeleteUser(cmd.Context(), u.ID); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "removed pending identity %s\n", email)
	return nil
}

// runUIGrant creates the identity if it is absent and makes it active with the
// given role. Bootstrapping a console needs this because an approval presupposes a
// request: on a fresh volume nobody has logged in yet, so there is no row to
// approve, and the operator is the one who should be let in.
func runUIGrant(cmd *cobra.Command, args []string) error {
	if done, err := inVolume(cmd); done {
		return err
	}
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
	if email == "" {
		return errors.New("email cannot be empty")
	}
	u, err := st.ByEmail(cmd.Context(), email)
	if err != nil {
		// First sight: the row is created pending by the SSO path, so create it
		// here the same way and then activate it — one code path for identity
		// creation, one extra step for the decision.
		u, err = st.EnsureSSOUser(cmd.Context(), email, email)
		if err != nil {
			return fmt.Errorf("create identity %q: %w", email, err)
		}
	}
	if err := st.SetStatus(cmd.Context(), u.ID, uistore.StatusActive, role); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "granted %s: %s, active\n", email, role)
	return nil
}

// runUIDisable revokes access without forgetting the person: the row stays, so a
// later approval is a role change rather than a fresh request.
func runUIDisable(cmd *cobra.Command, args []string) error {
	if done, err := inVolume(cmd); done {
		return err
	}
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
	if u.Role == uistore.RoleAdmin {
		if n, cerr := st.CountActiveAdmins(cmd.Context()); cerr == nil && n <= 1 {
			return fmt.Errorf("%s is the last active admin — approving someone else first is the way out of that", email)
		}
	}
	if err := st.SetStatus(cmd.Context(), u.ID, uistore.StatusDisabled, ""); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "disabled %s\n", email)
	return nil
}

// runUIDelete removes an identity outright, including active ones. The last-admin
// guard applies here too: deleting the only admin leaves a console nobody can
// administer, and the way out is a CLI approval, which is not obvious at 2am.
func runUIDelete(cmd *cobra.Command, args []string) error {
	if done, err := inVolume(cmd); done {
		return err
	}
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
	if u.Role == uistore.RoleAdmin && u.Status == uistore.StatusActive {
		if n, cerr := st.CountActiveAdmins(cmd.Context()); cerr == nil && n <= 1 {
			return fmt.Errorf("%s is the last active admin — approve someone else first", email)
		}
	}
	if err := st.DeleteUser(cmd.Context(), u.ID); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", email)
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
