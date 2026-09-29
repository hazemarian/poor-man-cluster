package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage DB-backed configs (cluster templates & service configs)",
	Long: `Configs live in the pmcluster database and are rendered into Docker
configs on 'cluster up' / 'cluster update'. Cluster-scoped configs are the
platform templates (traefik_dynamic, otel_collector, edge_stack, ...).
Service-scoped configs are user-created values referenced from the DSL:

  env:
    ADMIN_ENABLED: config(app_config)    # value injected into the container

Every edit preserves the previous value in history, so configs can be
rolled back with ` + "`pmcluster config rollback <name>`" + `.`,
}

var configCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new config (value from flag or stdin)",
	Args:  cobra.ExactArgs(1),
	RunE:  runConfigCreate,
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configs (name, scope, kind, hash, updated)",
	RunE:  runConfigList,
}

var configGetCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Print a config's value",
	Args:  cobra.ExactArgs(1),
	RunE:  runConfigGet,
}

var configEditCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Replace a config's value (from flag, $EDITOR, or stdin)",
	Args:  cobra.ExactArgs(1),
	RunE:  runConfigEdit,
}

var configRollbackCmd = &cobra.Command{
	Use:   "rollback <name> [version-id]",
	Short: "Restore a config to a previous version (default: previous)",
	Args:  cobra.MaximumNArgs(2),
	RunE:  runConfigRollback,
}

var configVersionsCmd = &cobra.Command{
	Use:   "history <name>",
	Short: "List a config's version history",
	Args:  cobra.ExactArgs(1),
	RunE:  runConfigHistory,
}

var configDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a config (removes the DB row)",
	Args:  cobra.ExactArgs(1),
	RunE:  runConfigDelete,
}

func init() {
	configCreateCmd.Flags().String("scope", "service", "scope: cluster or service")
	configCreateCmd.Flags().String("stack", "", "stack this config belongs to (service scope only)")
	configCreateCmd.Flags().String("kind", "file", "kind: template, file, or env")
	configCreateCmd.Flags().String("value", "", "config content (or pipe via stdin)")
	configEditCmd.Flags().String("value", "", "new config content (or pipe via stdin)")
	configCmd.AddCommand(configCreateCmd, configListCmd, configGetCmd, configEditCmd,
		configVersionsCmd, configRollbackCmd, configDeleteCmd)
	rootCmd.AddCommand(configCmd)
}

// configValueStdin is the stdin source for readConfigValue — overridable in
// tests; nil means os.Stdin.
var configValueStdin io.Reader

// statReader reports the file-info for a reader. A *os.File is inspected as
// real stdin (char-device detection); any other reader (injected in tests) is
// reported as a named pipe so the trim branch is exercised regardless of the
// test runner's real stdin.
func statReader(r io.Reader) (os.FileInfo, error) {
	if f, ok := r.(*os.File); ok {
		return f.Stat()
	}
	return pipeInfo{}, nil
}

// pipeInfo is a minimal FileInfo describing a named pipe (never a char
// device), so injected stdin readers are always treated as piped input.
type pipeInfo struct{}

func (pipeInfo) Name() string       { return "pipe" }
func (pipeInfo) Size() int64        { return 0 }
func (pipeInfo) Mode() os.FileMode  { return os.ModeNamedPipe }
func (pipeInfo) ModTime() time.Time { return time.Time{} }
func (pipeInfo) IsDir() bool        { return false }
func (pipeInfo) Sys() any           { return nil }

func readConfigValue(arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	in := configValueStdin
	if in == nil {
		in = os.Stdin
	}
	if fi, err := statReader(in); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		b, err := io.ReadAll(io.LimitReader(in, 4<<20))
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		// Piped input (echo/printf | pmcluster config edit) carries a trailing
		// newline; trim it so the stored config is exactly what the operator
		// typed. Environment values must be single-line — a raw newline would
		// later fail translation with 'config(...) contains newlines'.
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return "", errors.New("no value given — pass --value or pipe the content via stdin")
}

func runConfigCreate(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return errors.New("name: required")
	}
	scope, _ := cmd.Flags().GetString("scope")
	switch scope {
	case "cluster", "service":
	default:
		return fmt.Errorf("scope must be cluster or service, got %q", scope)
	}
	kind, _ := cmd.Flags().GetString("kind")
	switch kind {
	case "template", "file", "env":
	default:
		return fmt.Errorf("kind must be template, file, or env, got %q", kind)
	}
	stack, _ := cmd.Flags().GetString("stack")
	value, err := readConfigValue(cmd.Flag("value").Value.String())
	if err != nil {
		return err
	}

	svc, closeFn, err := backendConfigs(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	ver := buildinfo.Version
	if _, err := svc.Create(cmd.Context(), scope, stack, name, kind, value, ver); err != nil {
		if errors.Is(err, store.ErrConfigExists) {
			return fmt.Errorf("config %q already exists (use `pmcluster config edit %s`)", name, name)
		}
		return fmt.Errorf("create config: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✅ Config %q created (scope: %s, kind: %s).\n", name, scope, kind)
	return nil
}

func runConfigList(cmd *cobra.Command, _ []string) error {
	svc, closeFn, err := backendConfigs(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	cfgs, err := svc.List(cmd.Context(), "", "")
	if err != nil {
		return fmt.Errorf("list configs: %w", err)
	}
	if len(cfgs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no configs yet — run `pmcluster config create <name>`)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCOPE\tKIND\tHASH\tUPDATED")
	for _, c := range cfgs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			c.Name, c.Scope, c.Kind, shortHash(c.Hash),
			time.Unix(c.UpdatedAt, 0).Format(time.RFC3339))
	}
	return w.Flush()
}

func runConfigGet(cmd *cobra.Command, args []string) error {
	name := args[0]
	svc, closeFn, err := backendConfigs(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	c, err := svc.Get(cmd.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			return fmt.Errorf("config %q not found", name)
		}
		return fmt.Errorf("get config: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "# %s (scope: %s, kind: %s, hash: %s)\n%s",
		c.Name, c.Scope, c.Kind, shortHash(c.Hash), c.Content)
	if !strings.HasSuffix(c.Content, "\n") {
		fmt.Fprintln(cmd.OutOrStdout())
	}
	return nil
}

func runConfigEdit(cmd *cobra.Command, args []string) error {
	name := args[0]
	value, err := readConfigValue(cmd.Flag("value").Value.String())
	if err != nil {
		return err
	}

	svc, closeFn, err := backendConfigs(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	hash, err := svc.Update(cmd.Context(), name, value, buildinfo.Version)
	if err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			return fmt.Errorf("config %q not found", name)
		}
		return fmt.Errorf("update config: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✅ Config %q updated (hash: %s).\n", name, shortHash(hash))
	fmt.Fprintln(cmd.OutOrStdout(), "   Run `pmcluster cluster update` to apply it to the cluster.")
	return nil
}

func runConfigHistory(cmd *cobra.Command, args []string) error {
	name := args[0]
	svc, closeFn, err := backendConfigs(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	vers, err := svc.ListVersions(cmd.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			return fmt.Errorf("config %q not found", name)
		}
		return fmt.Errorf("list versions: %w", err)
	}
	if len(vers) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no history yet — this config has never been edited)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tHASH\tCREATED")
	for _, v := range vers {
		fmt.Fprintf(w, "%d\t%s\t%s\n", v.ID, shortHash(v.Hash),
			time.Unix(v.CreatedAt, 0).Format(time.RFC3339))
	}
	return w.Flush()
}

func runConfigRollback(cmd *cobra.Command, args []string) error {
	name := args[0]
	svc, closeFn, err := backendConfigs(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	var versionID int64
	if len(args) == 2 {
		versionID, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Errorf("version id must be an integer, got %q", args[1])
		}
	} else {

		vers, err := svc.ListVersions(cmd.Context(), name)
		if err != nil {
			if errors.Is(err, store.ErrConfigNotFound) {
				return fmt.Errorf("config %q not found", name)
			}
			return fmt.Errorf("list versions: %w", err)
		}
		if len(vers) == 0 {
			return fmt.Errorf("config %q has no previous versions to roll back to", name)
		}
		versionID = vers[0].ID
	}

	hash, err := svc.Rollback(cmd.Context(), name, versionID)
	if err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			return fmt.Errorf("config %q not found", name)
		}
		if errors.Is(err, store.ErrConfigVersionNotFound) {
			return fmt.Errorf("version %d not found for config %q", versionID, name)
		}
		return fmt.Errorf("rollback config: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✅ Config %q rolled back (hash: %s).\n", name, shortHash(hash))
	fmt.Fprintln(cmd.OutOrStdout(), "   Run `pmcluster cluster update` to apply it to the cluster.")
	return nil
}

// runConfigDelete removes a config row entirely. Unlike update/rollback it
// drops the value AND its version history — the next cluster update simply
// stops rendering that config.
func runConfigDelete(cmd *cobra.Command, args []string) error {
	name := args[0]
	svc, closeFn, err := backendConfigs(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := svc.Delete(cmd.Context(), name); err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			return fmt.Errorf("config %q not found", name)
		}
		return fmt.Errorf("delete config: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Config %q deleted.\n", name)
	return nil
}
