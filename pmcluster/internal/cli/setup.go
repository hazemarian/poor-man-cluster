package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// setupCmd is an interactive wizard that collects the cluster configuration
// (domain, TLS mode, OpenObserve admin, SSO, edge login) into the local store
// settings and then hands off to `cluster up` (fresh install) or
// `cluster update` (existing cluster). Every answer is also available as a
// flag for non-interactive/scripted runs.
var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Interactive wizard: collect cluster config then run cluster up/update",
	Long: `Walks you through the cluster configuration questions and persists the
answers into the store settings:

  - Cluster domain (e.g. example.com)
  - TLS: Let's Encrypt (--acme-email) or operator-supplied cert/key (--cert/--key)
  - Traefik admin user
  - SSO: enable GitHub sign-in via an OAuth2 proxy (--sso-enabled,
    --sso-client-id, --sso-client-secret, --sso-github-org)
  - Edge console login (EDGE_LOGIN_DISABLED)

The OpenObserve admin email is derived automatically (admin@<domain>) — it is
hidden behind the admin-auth / SSO gate, so there is no prompt for it.

On a fresh install it then runs 'pmcluster cluster up'; on an existing
cluster it runs 'pmcluster cluster update'. Every question has a matching
flag so the wizard can be skipped entirely in scripts.`,
	RunE: runSetup,
}

func init() {
	rootCmd.AddCommand(setupCmd)

	// Non-interactive flags mirror every prompt (see runSetup).
	setupCmd.Flags().String("domain", "", "cluster domain (e.g. example.com)")
	setupCmd.Flags().String("acme-email", "", "Let's Encrypt contact email (enables ACME)")
	setupCmd.Flags().String("cert", "", "operator-supplied TLS certificate PEM path")
	setupCmd.Flags().String("key", "", "operator-supplied TLS key PEM path")
	setupCmd.Flags().String("openobserve-email", "", "OpenObserve admin email (default admin@<domain>)")
	setupCmd.Flags().String("traefik-admin-user", "", "Traefik dashboard admin user")
	setupCmd.Flags().Bool("sso-enabled", false, "enable GitHub SSO via OAuth2 proxy")
	setupCmd.Flags().String("sso-client-id", "", "GitHub OAuth app client ID (SSO)")
	setupCmd.Flags().String("sso-client-secret", "", "GitHub OAuth app client secret (SSO)")
	setupCmd.Flags().String("sso-github-org", "", "restrict SSO to a GitHub org (optional)")
	setupCmd.Flags().Bool("edge-login-enabled", false, "keep the edge console password login (default: disabled behind SSO/admin-auth)")
}

// setupAnswers is the collected wizard state.
type setupAnswers struct {
	Domain           string
	ACMEEmail        string
	CertPath         string
	KeyPath          string
	OpenObserveEmail string
	TraefikAdminUser string

	SSOEnabled      bool
	SSOProvider     string
	SSOClientID     string
	SSOClientSecret string
	SSOGitHubOrg    string

	EdgeLoginEnabled bool
}

// ask prompts for a free-form value with a default; returns the trimmed answer.
func ask(r *bufio.Reader, out io.Writer, prompt, def string) string {
	label := prompt
	if def != "" {
		label = fmt.Sprintf("%s [%s]", prompt, def)
	}
	fmt.Fprintf(out, "%s: ", label)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// askYesNo prompts for a yes/no answer; returns def when the input is empty.
func askYesNo(r *bufio.Reader, out io.Writer, prompt string, def bool) bool {
	defLabel := "n"
	if def {
		defLabel = "y"
	}
	for {
		ans := strings.ToLower(ask(r, out, fmt.Sprintf("%s (y/n)", prompt), defLabel))
		switch ans {
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Fprintln(out, "  (answer y or n)")
	}
}

func runSetup(cmd *cobra.Command, _ []string) error {
	defer initCLITelemetry()()

	ctx := cmd.Context()

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

	out := cmd.OutOrStdout()
	interactive := !cmd.Flags().Changed("domain") && isTTY(cmd)

	var a setupAnswers
	if interactive {
		r := bufio.NewReader(os.Stdin)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "┌──────────────────────────────────────────────┐")
		fmt.Fprintln(out, "│  pmcluster setup — cluster configuration      │")
		fmt.Fprintln(out, "└──────────────────────────────────────────────┘")

		a.Domain = ask(r, out, "Cluster domain", st.GetSettingDefault(ctx, cluster.SettingDomain(), ""))
		le := askYesNo(r, out, "Use Let's Encrypt?", a.ACMEEmail != "")
		if le {
			a.ACMEEmail = ask(r, out, "ACME contact email", st.GetSettingDefault(ctx, cluster.SettingTLSACME(), ""))
		} else {
			a.CertPath = ask(r, out, "TLS certificate PEM path", "")
			a.KeyPath = ask(r, out, "TLS key PEM path", "")
		}
		a.OpenObserveEmail = defaultOOEmail(a.Domain)
		a.TraefikAdminUser = ask(r, out, "Traefik admin user", "admin")
		a.SSOEnabled = askYesNo(r, out, "Enable SSO (GitHub)?", st.GetSettingDefault(ctx, cluster.SettingSSOEnabled(), "") == "true")
		if a.SSOEnabled {
			a.SSOProvider = ask(r, out, "SSO provider", "github")
			a.SSOClientID = ask(r, out, "GitHub OAuth client ID", "")
			a.SSOClientSecret = ask(r, out, "GitHub OAuth client secret", "")
			a.SSOGitHubOrg = ask(r, out, "Restrict to GitHub org (optional)", st.GetSettingDefault(ctx, cluster.SettingSSOGitHubOrg(), ""))
		}
		a.EdgeLoginEnabled = askYesNo(r, out, "Keep edge console password login?", false)
	} else {
		a.Domain, _ = cmd.Flags().GetString("domain")
		a.ACMEEmail, _ = cmd.Flags().GetString("acme-email")
		a.CertPath, _ = cmd.Flags().GetString("cert")
		a.KeyPath, _ = cmd.Flags().GetString("key")
		a.OpenObserveEmail, _ = cmd.Flags().GetString("openobserve-email")
		if a.OpenObserveEmail == "" {
			// Hidden behind the admin-auth/SSO gate — no need to prompt.
			a.OpenObserveEmail = defaultOOEmail(a.Domain)
		}
		a.TraefikAdminUser, _ = cmd.Flags().GetString("traefik-admin-user")
		a.SSOEnabled, _ = cmd.Flags().GetBool("sso-enabled")
		a.SSOClientID, _ = cmd.Flags().GetString("sso-client-id")
		a.SSOClientSecret, _ = cmd.Flags().GetString("sso-client-secret")
		a.SSOGitHubOrg, _ = cmd.Flags().GetString("sso-github-org")
		a.EdgeLoginEnabled, _ = cmd.Flags().GetBool("edge-login-enabled")
		if a.SSOEnabled {
			a.SSOProvider = "github"
		}
	}

	// Persist the collected answers into the store settings.
	if a.Domain == "" {
		return fmt.Errorf("domain is required (interactive prompt or --domain)")
	}
	if a.SSOEnabled && (a.SSOClientID == "" || a.SSOClientSecret == "") {
		return fmt.Errorf("SSO enabled but client ID/secret missing")
	}
	if a.ACMEEmail == "" && (a.CertPath == "" || a.KeyPath == "") {
		return fmt.Errorf("TLS mode required: either --acme-email or --cert/--key")
	}

	// The installed check must run BEFORE persisting — the wizard's own
	// settings would otherwise make a fresh install look like an existing
	// cluster.
	installed := cluster.ClusterInstalled(ctx, st)

	if installed {
		// Existing cluster: persist everything, then reconcile.
		if err := persistSetup(ctx, st, a); err != nil {
			return err
		}
		fmt.Fprintln(out, "Cluster already initialised — running `pmcluster cluster update` with the new settings.")
		return runClusterUpdate(cmd, nil)
	}

	// Fresh install: persist ONLY the settings Up's render step reads from the
	// store (SSO + edge login). Domain / OO email / TLS state must NOT be
	// persisted here — Up's own clusterInstalled guard would then reject the
	// install — so they are handed over through UpInput and Up persists them
	// itself via persistInstallState.
	if err := persistSetupSecretsOnly(ctx, st, a); err != nil {
		return err
	}

	in := cluster.UpInput{}
	in.Version = buildinfo.Version
	in.Domain = a.Domain
	in.ACMEEmail = a.ACMEEmail
	in.CertPath = a.CertPath
	in.KeyPath = a.KeyPath
	in.OpenObserveAdminEmail = a.OpenObserveEmail
	in.TraefikAdminUser = a.TraefikAdminUser
	in.ConfigDir = cfg.ConfigDir()

	fmt.Fprintln(out, "Fresh install — running `pmcluster cluster up` with the new settings.")
	return runUp(cmd, cfg, in)
}

// defaultOOEmail derives the OpenObserve root admin email from the cluster
// domain (admin@<domain>). The email is hidden behind the admin-auth / SSO
// gate and only used for the OO root user, so it is not worth prompting for.
func defaultOOEmail(domain string) string {
	if domain == "" {
		return ""
	}
	return "admin@" + domain
}

// persistSetupSecretsOnly persists the settings the Up/Update render pipeline
// reads from the store (SSO + edge login). Domain / OO email / TLS state are
// intentionally excluded: on a fresh install Up receives them through
// UpInput and persists them itself.
func persistSetupSecretsOnly(ctx context.Context, st *store.Store, a setupAnswers) error {
	setting := map[string]string{
		cluster.SettingSSOEnabled():        boolSetting(a.SSOEnabled),
		cluster.SettingSSOProvider():       a.SSOProvider,
		cluster.SettingSSOClientID():       a.SSOClientID,
		cluster.SettingSSOClientSecret():   a.SSOClientSecret,
		cluster.SettingSSOGitHubOrg():      a.SSOGitHubOrg,
		cluster.SettingEdgeLoginDisabled(): boolSetting(!a.EdgeLoginEnabled),
	}
	for k, v := range setting {
		if err := st.SetSetting(ctx, k, v); err != nil {
			return fmt.Errorf("save setting %s: %w", k, err)
		}
	}
	return nil
}

// persistSetup writes every collected answer into the store settings.
func persistSetup(ctx context.Context, st *store.Store, a setupAnswers) error {
	setting := map[string]string{
		cluster.SettingDomain():            a.Domain,
		cluster.SettingOOEmail():           a.OpenObserveEmail,
		cluster.SettingTraefikAdminUser():  a.TraefikAdminUser,
		cluster.SettingSSOEnabled():        boolSetting(a.SSOEnabled),
		cluster.SettingSSOProvider():       a.SSOProvider,
		cluster.SettingSSOClientID():       a.SSOClientID,
		cluster.SettingSSOClientSecret():   a.SSOClientSecret,
		cluster.SettingSSOGitHubOrg():      a.SSOGitHubOrg,
		cluster.SettingEdgeLoginDisabled(): boolSetting(!a.EdgeLoginEnabled),
	}
	for k, v := range setting {
		if err := st.SetSetting(ctx, k, v); err != nil {
			return fmt.Errorf("save setting %s: %w", k, err)
		}
	}

	// TLS state: ACME or operator cert/key.
	tls := cluster.TLSState{
		Mode:      "operator",
		CertPath:  a.CertPath,
		KeyPath:   a.KeyPath,
		ACMEEmail: a.ACMEEmail,
	}
	if a.ACMEEmail != "" {
		tls.Mode = "acme"
	}
	if err := tls.Save(ctx, st); err != nil {
		return fmt.Errorf("save TLS state: %w", err)
	}
	return nil
}

func boolSetting(b bool) string {
	if b {
		return "true"
	}
	return ""
}

// isTTY reports whether the command's stdout is a terminal (interactive).
func isTTY(cmd *cobra.Command) bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
