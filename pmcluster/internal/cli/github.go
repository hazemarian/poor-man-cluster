package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The GitHub App surface. The app is created by hand in GitHub (Settings →
// Developer settings → GitHub Apps); pmcluster stores its id and private key,
// then mints one-hour installation tokens on demand. There is deliberately no
// App Manifest browser flow here.
//
// TODO(app-manifest): if the manual creation ever becomes the friction point,
// the browser flow is three endpoints: POST /api/github/app/manifest (create
// the app from a manifest, returns a one-time code), GET /api/github/app/manifest/callback
// (exchange the code, store id/key), and POST /api/github/app/install
// (redirect to the installation URL). Do not build it until the operator asks:
// it needs a public redirect URL and app-owner rights the cluster may not have.

var githubCmd = &cobra.Command{
	Use:   "github",
	Short: "GitHub App credentials and the Actions token broker",
	Long: `The cluster can hold one GitHub App instead of personal access tokens:

  - its installation tokens are the GHCR password for private image pulls;
  - GitHub Actions workflows mint one-hour tokens from this daemon (per-repo
    HMAC secrets) instead of storing secrets.GH_PAT.

The App is created manually in GitHub; this command stores its id and private
key encrypted in the control-plane database. Per-repo broker secrets are stored
the same way and shown exactly once, at creation.`,
}

var githubAppCmd = &cobra.Command{
	Use:   "app",
	Short: "Store, inspect or clear the GitHub App registration",
}

var githubBrokerCmd = &cobra.Command{
	Use:   "broker",
	Short: "Manage per-repository secrets for POST /api/github/token",
}

var githubAppSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Store the app id and private key (encrypted)",
	Long: `Examples:
  pmcluster github app set --app-id 123456 --key-file ~/pmcluster-app.pem --installation-id 789
  pmcluster github app set --app-id 123456 --key-file app.pem \
      --probe-repo acme/app --probe-package acme/app:latest

--key-file is required the first time. Afterwards, omitting it keeps the stored
key while changing the ids or probe targets. --installation-id may be omitted
until the App is installed; find it in the installation's settings URL
(github.com/organizations/<org>/settings/installations/<id>).`,
	Args: cobra.NoArgs,
	RunE: runGitHubAppSet,
}

var githubAppShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the app id, installation id and probe targets (never the key)",
	Args:  cobra.NoArgs,
	RunE:  runGitHubAppShow,
}

var githubAppClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Remove the app registration (broker secrets are kept)",
	Args:  cobra.NoArgs,
	RunE:  runGitHubAppClear,
}

var githubAppTestCmd = &cobra.Command{
	Use:   "test",
	Short: "Mint a token and probe the configured repo/package (live GitHub call)",
	Long: `This is a live call to GitHub: it mints an installation token and, when
--probe-repo / --probe-package are configured, reads that repository and HEADs
that GHCR manifest. It is the same measurement ` + "`pmcluster doctor`" + ` makes.`,
	Args: cobra.NoArgs,
	RunE: runGitHubAppTest,
}

var githubBrokerAddCmd = &cobra.Command{
	Use:   "add <owner/repo>",
	Short: "Create a broker secret for one repository (printed once)",
	Args:  cobra.ExactArgs(1),
	RunE:  runGitHubBrokerAdd,
}

var githubBrokerRotateCmd = &cobra.Command{
	Use:   "rotate <owner/repo>",
	Short: "Replace one repository's broker secret (printed once)",
	Args:  cobra.ExactArgs(1),
	RunE:  runGitHubBrokerRotate,
}

var githubBrokerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List repositories that may mint tokens (never the secrets)",
	RunE:  runGitHubBrokerList,
}

var githubBrokerRemoveCmd = &cobra.Command{
	Use:   "remove <owner/repo>",
	Short: "Revoke one repository's broker access",
	Args:  cobra.ExactArgs(1),
	RunE:  runGitHubBrokerRemove,
}

func init() {
	// Numeric ids are STRING flags on purpose: docscheck's flag extractor
	// only sees alphabetic pflag methods (String/Bool), so an Int64 flag would
	// be invisible to the documentation gate — and a flag the gate cannot see
	// is a flag a doc can name wrongly forever.
	githubAppSetCmd.Flags().String("app-id", "", "GitHub App id (required)")
	githubAppSetCmd.Flags().String("key-file", "", "path to the app's private key .pem (required on first configuration)")
	githubAppSetCmd.Flags().String("installation-id", "", "installation id on the org (optional)")
	githubAppSetCmd.Flags().String("probe-repo", "", "owner/repo doctor and the console Test button read to prove contents:read")
	githubAppSetCmd.Flags().String("probe-package", "", "owner/name:tag on GHCR the doctor HEADs to prove packages:read")

	githubBrokerAddCmd.Flags().String("description", "", "what this secret is for")
	githubBrokerRotateCmd.Flags().String("description", "", "what this secret is for")

	githubAppCmd.AddCommand(githubAppSetCmd, githubAppShowCmd, githubAppClearCmd, githubAppTestCmd)
	githubBrokerCmd.AddCommand(githubBrokerAddCmd, githubBrokerRotateCmd, githubBrokerListCmd, githubBrokerRemoveCmd)
	githubCmd.AddCommand(githubAppCmd, githubBrokerCmd)
	rootCmd.AddCommand(githubCmd)
}

func runGitHubAppSet(cmd *cobra.Command, _ []string) error {
	appID, err := int64Flag(cmd, "app-id")
	if err != nil {
		return err
	}
	if appID <= 0 {
		return errors.New("--app-id is required and must be positive")
	}
	keyFile, _ := cmd.Flags().GetString("key-file")
	var keyPEM []byte
	if keyFile != "" {
		data, err := os.ReadFile(keyFile)
		if err != nil {
			return fmt.Errorf("read --key-file: %w", err)
		}
		keyPEM = data
	}
	installationID, err := int64Flag(cmd, "installation-id")
	if err != nil {
		return err
	}
	probeRepo, _ := cmd.Flags().GetString("probe-repo")
	probePackage, _ := cmd.Flags().GetString("probe-package")

	l, closeStore, err := openGitHubApp()
	if err != nil {
		return err
	}
	defer closeStore()

	if err := l.Save(cmd.Context(), appID, installationID, keyPEM, probeRepo, probePackage); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ GitHub App %d stored (installation %d).\n", appID, installationID)
	fmt.Fprintln(cmd.OutOrStdout(), "  The private key is encrypted in data.db and is never displayed again.")
	if installationID == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "  No installation id yet: install the App on the org and copy the id from github.com/organizations/<org>/settings/installations/<id>, then set --installation-id.")
	}
	if probeRepo == "" || probePackage == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "  Set --probe-repo and --probe-package so `pmcluster doctor` can prove contents:read and packages:read.")
	}
	return nil
}

func runGitHubAppShow(cmd *cobra.Command, _ []string) error {
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	row, err := st.GitHubApp(cmd.Context())
	if err != nil {
		if errors.Is(err, store.ErrGitHubAppNotConfigured) {
			fmt.Fprintln(cmd.OutOrStdout(), "(no GitHub App configured — use `pmcluster github app set --app-id … --key-file …`)")
			return nil
		}
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "APP ID\t%d\n", row.AppID)
	fmt.Fprintf(w, "INSTALLATION ID\t%d\n", row.InstallationID)
	fmt.Fprintf(w, "PROBE REPO\t%s\n", orDash(row.ProbeRepo))
	fmt.Fprintf(w, "PROBE PACKAGE\t%s\n", orDash(row.ProbePackage))
	fmt.Fprintf(w, "UPDATED\t%s\n", time.Unix(row.UpdatedAt, 0).UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "PRIVATE KEY\t%s\n", "stored encrypted (not displayed)")
	return w.Flush()
}

func runGitHubAppClear(cmd *cobra.Command, _ []string) error {
	l, closeStore, err := openGitHubApp()
	if err != nil {
		return err
	}
	defer closeStore()
	if err := l.Clear(cmd.Context()); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "✅ GitHub App registration removed. Per-repo broker secrets were kept; remove them with `pmcluster github broker remove`.")
	return nil
}

func runGitHubAppTest(cmd *cobra.Command, _ []string) error {
	l, closeStore, err := openGitHubApp()
	if err != nil {
		return err
	}
	defer closeStore()
	res := l.Test(cmd.Context())
	mark := "✅"
	if !res.OK {
		mark = "✖"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", mark, res.Message)
	if !res.OK {
		return errors.New("github app test failed")
	}
	return nil
}

func runGitHubBrokerAdd(cmd *cobra.Command, args []string) error {
	return createBrokerSecret(cmd, args[0], false)
}

func runGitHubBrokerRotate(cmd *cobra.Command, args []string) error {
	return createBrokerSecret(cmd, args[0], true)
}

func createBrokerSecret(cmd *cobra.Command, repo string, rotate bool) error {
	desc, _ := cmd.Flags().GetString("description")
	l, closeStore, err := openGitHubApp()
	if err != nil {
		return err
	}
	defer closeStore()

	var secret string
	if rotate {
		secret, err = l.RotateBrokerSecret(cmd.Context(), repo, desc)
	} else {
		secret, err = l.CreateBrokerSecret(cmd.Context(), repo, desc)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Broker secret for %s:\n\n  %s\n\n", repo, secret)
	fmt.Fprintln(cmd.OutOrStdout(), "Store it as a repository secret (e.g. PMCLUSTER_BROKER_SECRET). It is not shown again.")
	return nil
}

func runGitHubBrokerList(cmd *cobra.Command, _ []string) error {
	l, closeStore, err := openGitHubApp()
	if err != nil {
		return err
	}
	defer closeStore()
	rows, err := l.ListBrokerSecrets(cmd.Context())
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no broker secrets — use `pmcluster github broker add owner/repo`)")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tDESCRIPTION\tLAST USED")
	for _, r := range rows {
		last := "never"
		if r.LastUsedAt > 0 {
			last = time.Unix(r.LastUsedAt, 0).UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Repo, orDash(r.Description), last)
	}
	return w.Flush()
}

func runGitHubBrokerRemove(cmd *cobra.Command, args []string) error {
	l, closeStore, err := openGitHubApp()
	if err != nil {
		return err
	}
	defer closeStore()
	if err := l.DeleteBrokerSecret(cmd.Context(), args[0]); err != nil {
		if errors.Is(err, store.ErrGitHubBrokerSecretNotFound) {
			return fmt.Errorf("no broker secret for %q", args[0])
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Broker access revoked for %s.\n", args[0])
	return nil
}

// openGitHubApp wires the store, the cipher and the API client for a one-shot
// CLI command. The caller owns the returned close func.
func openGitHubApp() (*githubapp.Local, func(), error) {
	st, cfg, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		_ = st.Close()
		return nil, nil, fmt.Errorf("open encryption key: %w", err)
	}
	l := &githubapp.Local{
		Store:  st,
		Cipher: cipher,
		Client: &githubapp.Client{},
	}
	return l, func() { _ = st.Close() }, nil
}

// newGitHubClient is the daemon/CLI API client. A package variable so tests can
// point it at an httptest server; production never writes to it.
var newGitHubClient = func(log *zerolog.Logger) *githubapp.Client {
	return &githubapp.Client{Log: log}
}

// newGitHubAppLocal is the daemon-side constructor, which also wires the
// logger. Kept next to the CLI version so both use the same defaults.
func newGitHubAppLocal(st *store.Store, cipher *credentials.Cipher, log zerolog.Logger) *githubapp.Local {
	return &githubapp.Local{
		Store:  st,
		Cipher: cipher,
		Client: newGitHubClient(&log),
		Log:    &log,
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// int64Flag reads a numeric flag declared as a string. An empty value is 0;
// anything unparseable is named rather than silently ignored.
func int64Flag(cmd *cobra.Command, name string) (int64, error) {
	raw, _ := cmd.Flags().GetString(name)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("--%s must be a number, got %q", name, raw)
	}
	return v, nil
}
