package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/remote"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/settings"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/usage"
)

var clusterSettingsCmd = &cobra.Command{
	Use:   "settings",
	Short: "Read or update cluster settings (volume_root, backup_*, sso_*)",
	Long: `Reads or writes the cluster_settings table in the daemon store.

` + "`pmcluster cluster settings`" + `          list all settings
` + "`pmcluster cluster settings get <key>`" + `  show one setting
` + "`pmcluster cluster settings set <key>=<value>`" + `  update one or more settings

Secret-typed keys (anything whose name contains secret, password, key, token or
credential) are printed as ` + "`" + `settings.Mask` + "`" + `, the same policy the API and the
console apply. Pass --show-secrets when you actually need the value — and expect
it in your shell history, your paste buffer and anything you pipe this into.

Settings are applied to the swarm by ` + "`pmcluster cluster update`" + ` (the settings
command itself only persists them).`,
	RunE: runClusterSettings,
}

func init() {
	clusterSettingsCmd.Flags().Bool("show-secrets", false,
		"print secret-typed settings in cleartext (they land in your shell history)")
	clusterCmd.AddCommand(clusterSettingsCmd)
}

// showSecrets reports whether the caller asked for cleartext secret values.
func showSecrets(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool("show-secrets")
	return err == nil && v
}

// backendSettings resolves the settings service, and the LOCAL store when there is one.
// The store comes back so the caller can label a mirrored answer with its age — the same
// table is the leader's source of truth and a follower's mirror, and the difference has to
// be visible on stdout.
func backendSettings(cmd *cobra.Command) (settings.Service, *store.Store, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewClusterSettings(rc), nil, func() {}, nil
	}
	st, _, err := openStore()
	if err != nil {
		return nil, nil, nil, err
	}
	return settings.NewLocal(st), st, func() { _ = st.Close() }, nil
}

func backendUsage(cmd *cobra.Command) (usage.Service, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewUsage(rc), func() {}, nil
	}
	st, _, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	return usage.NewLocal(st), func() { _ = st.Close() }, nil
}

func runClusterSettings(cmd *cobra.Command, args []string) error {
	defer initCLITelemetry()()

	svc, st, closeFn, err := backendSettings(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	// A follower answers from its mirror. Say whose state it is and how old before the
	// values, or the reader cannot tell a fresh answer from one the leader stopped
	// refreshing an hour ago — which is the exact confusion this migration removes.
	if isRead := len(args) == 0 || (len(args) > 0 && args[0] == "get"); isRead && st != nil {
		if _, onFollower := followerSelf(cmd.Context()); onFollower {
			printMirrorLabel(cmd, st)
		}
	}

	// get <key> — print a single value.
	if len(args) == 1 {
		if args[0] == "get" {
			return fmt.Errorf("usage: pmcluster cluster settings get <key>")
		}
		return fmt.Errorf("usage: pmcluster cluster settings [get <key> | set key=value ...]")
	}

	if len(args) >= 2 && args[0] == "get" {
		all, err := svc.Get(cmd.Context())
		if err != nil {
			return fmt.Errorf("get settings: %w", err)
		}
		v, ok := all[args[1]]
		if !ok {
			return fmt.Errorf("unknown setting %q", args[1])
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", args[1], settings.MaskValue(args[1], v, showSecrets(cmd)))
		return nil
	}

	if len(args) >= 2 && args[0] == "set" {
		values := settings.Settings{}
		for _, kv := range args[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return fmt.Errorf("set expects key=value pairs (got %q)", kv)
			}
			values[k] = v
		}
		updated, err := svc.Update(cmd.Context(), values)
		if err != nil {
			return fmt.Errorf("update settings: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "✅ Settings updated. Run `pmcluster cluster update` to apply them to the swarm.")
		return printSettings(cmd, updated, showSecrets(cmd))
	}

	// No args — list everything.
	all, err := svc.Get(cmd.Context())
	if err != nil {
		return fmt.Errorf("get settings: %w", err)
	}
	return printSettings(cmd, all, showSecrets(cmd))
}

// printMirrorLabel names the leader and the age of the answer that follows.
//
// It never fails the command: an unreadable mirror means the values below are this node's
// own copy, and saying THAT is still better than printing them as if they were current.
func printMirrorLabel(cmd *cobra.Command, st *store.Store) {
	// cmd.Context() is nil on a command that was not executed through cobra's Execute (a
	// test, or a helper called directly), and a nil context does not fall back — it panics
	// inside database/sql on ctx.Done(). Ask for one explicitly.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	m, err := st.MirrorReading(ctx, time.Now())
	if err != nil {
		fmt.Fprint(cmd.OutOrStdout(),
			"mirrored: no snapshot applied on this node yet — the values below are this node's own copy and may be out of date\n\n")
		return
	}
	label := fmt.Sprintf("mirrored: %s (age_seconds=%d)", m.Human(), m.AgeSeconds)
	if content := m.ContentAge(); content != "" {
		label += " · " + content
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n", label)
}

func printSettings(cmd *cobra.Command, s settings.Settings, showSecrets bool) error {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tVALUE")
	masked := 0
	for _, k := range keys {
		v := s[k]
		if v == "" {
			v = "—"
		} else {
			// Root on the box can read the DB anyway, but stdout does not stay on
			// the box: it lands in shell history, in scrollback, in a pasted
			// transcript. So the CLI applies the SAME policy the API and console
			// apply, and cleartext is an explicit request.
			if settings.IsSecretKey(k) && !showSecrets {
				masked++
			}
			v = settings.MaskValue(k, v, showSecrets)
		}
		fmt.Fprintf(w, "%s\t%s\n", k, v)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if masked > 0 {
		fmt.Fprintf(cmd.OutOrStdout(),
			"\n%d secret-typed value(s) masked. `--show-secrets` prints them (they land in your shell history).\n", masked)
	}
	return nil
}

var usageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show which stacks reference each config and secret",
	Long: `Reads the latest rendered compose of every deployed stack and reports
which stacks mount each DB-backed config and swarm secret.`,
	RunE: runUsage,
}

func init() {
	rootCmd.AddCommand(usageCmd)
}

func runUsage(cmd *cobra.Command, _ []string) error {
	defer initCLITelemetry()()

	svc, closeFn, err := backendUsage(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	u, err := svc.Get(cmd.Context())
	if err != nil {
		return fmt.Errorf("get usage: %w", err)
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CONFIG\tUSED BY")
	for _, name := range sortedUsageKeys(u.Configs) {
		fmt.Fprintf(w, "%s\t%s\n", name, strings.Join(u.Configs[name], ", "))
	}
	if len(u.Configs) == 0 {
		fmt.Fprintln(w, "(no config references found)")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "SECRET\tUSED BY")
	for _, name := range sortedUsageKeys(u.Secrets) {
		fmt.Fprintf(w, "%s\t%s\n", name, strings.Join(u.Secrets[name], ", "))
	}
	if len(u.Secrets) == 0 {
		fmt.Fprintln(w, "(no secret references found)")
	}
	return w.Flush()
}

func sortedUsageKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
