package cli

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/remote"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/settings"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/usage"
)

var clusterSettingsCmd = &cobra.Command{
	Use:   "settings",
	Short: "Read or update cluster settings (volume_root, backup_all_nodes, sso_*)",
	Long: `Reads or writes the cluster_settings table in the daemon store.

` + "`pmcluster cluster settings`" + `          list all settings
` + "`pmcluster cluster settings get <key>`" + `  show one setting
` + "`pmcluster cluster settings set <key>=<value>`" + `  update one or more settings

Settings are applied to the swarm by ` + "`pmcluster cluster update`" + ` (the settings
command itself only persists them).`,
	RunE: runClusterSettings,
}

func init() {
	clusterCmd.AddCommand(clusterSettingsCmd)
}

func backendSettings(cmd *cobra.Command) (settings.Service, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewClusterSettings(rc), func() {}, nil
	}
	st, _, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	return settings.NewLocal(st), func() { _ = st.Close() }, nil
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

	svc, closeFn, err := backendSettings(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

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
		fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", args[1], v)
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
		return printSettings(cmd, updated)
	}

	// No args — list everything.
	all, err := svc.Get(cmd.Context())
	if err != nil {
		return fmt.Errorf("get settings: %w", err)
	}
	return printSettings(cmd, all)
}

func printSettings(cmd *cobra.Command, s settings.Settings) error {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tVALUE")
	for _, k := range keys {
		v := s[k]
		if strings.Contains(k, "secret") && v != "" {
			v = "********"
		}
		if v == "" {
			v = "—"
		}
		fmt.Fprintf(w, "%s\t%s\n", k, v)
	}
	return w.Flush()
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
