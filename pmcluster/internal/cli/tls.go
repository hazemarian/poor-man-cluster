package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service/impl"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// tlsCmd groups the per-host TLS certificate commands.
var tlsCmd = &cobra.Command{
	Use:   "tls",
	Short: "Manage per-host TLS certificates served by Traefik",
	Long: `A customer's own domain can serve the same app that lives on a
pmcluster subdomain, on its own origin with its own real certificate. These
commands store per-host certs as versioned Swarm secrets + a site_certs DB
row (the SAME table and flow as the cluster's own certificate) and refresh
Traefik so it serves them for the matching SNI.

The cluster's own default wildcard cert is managed with 'pmcluster tls site'
(set via 'pmcluster cluster up --cert/--key') and is never touched by the
'pmcluster tls hosts' commands.`,
}

var tlsHostsCmd = &cobra.Command{
	Use:   "hosts",
	Short: "Per-host TLS certificate store",
}

// tlsSiteCmd manages the cluster's OWN (main-domain) TLS certificate — the
// default cert served by Traefik for the cluster's domain (e.g. nextrum-sy.com).
var tlsSiteCmd = &cobra.Command{
	Use:   "site",
	Short: "Manage the cluster's own TLS certificate",
	Long: `Manages the certificate Traefik serves for the cluster's own domain
(the one persisted by 'pmcluster cluster up --domain').

  pmcluster tls site              show the current certificate metadata
  pmcluster tls site set --cert-file cert.pem --key-file key.pem
  pmcluster tls site set --cert "$(cat cert.pem)" --key "$(cat key.pem)"

Uploading validates the pair (PEM match + the cert must cover the cluster's
domain), stores a local copy under ~/.pmcluster/config/site/, re-materializes
the cert_vN/key_vN Swarm secrets, refreshes Traefik and records the metadata
(expiry, hashes) for monitoring.`,
}

var tlsSiteShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the cluster's own TLS certificate metadata",
	RunE:  runTLSSiteShow,
}

type tlsSiteSetFlags struct {
	certFile string
	keyFile  string
	cert     string
	key      string
}

var tlsSiteSetBinds tlsSiteSetFlags

var tlsSiteSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Upload a new certificate for the cluster's own domain",
	Args:  cobra.NoArgs,
	RunE:  runTLSSiteSet,
}

type tlsAddFlags struct {
	certFile  string
	keyFile   string
	cert      string
	key       string
	noRefresh bool
}

var tlsAddBinds tlsAddFlags

var tlsAddCmd = &cobra.Command{
	Use:   "add <host>",
	Short: "Store a per-host certificate (text) and refresh Traefik",
	Long: `Stores cert+key for <host> as versioned Swarm secrets
(hostcert-<host>_vNNN / hostkey-<host>_vNNN) with a site_certs DB metadata
row — the same table and flow as the cluster's own certificate. Per-host
certs are never written to the manager's filesystem.

The cert/key are provided as TEXT (paste them inline) or from PEM files:

  pmcluster tls hosts add idlibookfair.com --cert "$(cat cert.pem)" --key "$(cat key.pem)"
  pmcluster tls hosts add idlibookfair.com --cert-file cert.pem --key-file key.pem

After storing, pmcluster re-renders the Traefik dynamic config and re-deploys
the infra stack so the new cert is served. Use --no-refresh to only store the
cert (it takes effect on the next 'pmcluster cluster update').`,
	Args: cobra.ExactArgs(1),
	RunE: runTLSAdd,
}

var tlsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List stored per-host certificates",
	RunE:  runTLSList,
}

var tlsRemoveCmd = &cobra.Command{
	Use:   "remove <host>",
	Short: "Remove a stored per-host certificate and refresh Traefik",
	Args:  cobra.ExactArgs(1),
	RunE:  runTLSRemove,
}

func init() {
	tlsAddCmd.Flags().StringVar(&tlsAddBinds.certFile, "cert-file", "", "path to the certificate PEM file")
	tlsAddCmd.Flags().StringVar(&tlsAddBinds.keyFile, "key-file", "", "path to the private key PEM file")
	tlsAddCmd.Flags().StringVar(&tlsAddBinds.cert, "cert", "", "certificate PEM text")
	tlsAddCmd.Flags().StringVar(&tlsAddBinds.key, "key", "", "private key PEM text")
	tlsAddCmd.Flags().BoolVar(&tlsAddBinds.noRefresh, "no-refresh", false, "store only; defer the Traefik refresh to `cluster update`")

	tlsHostsCmd.AddCommand(tlsAddCmd, tlsListCmd, tlsRemoveCmd)
	tlsCmd.AddCommand(tlsHostsCmd)
	tlsSiteSetCmd.Flags().StringVar(&tlsSiteSetBinds.certFile, "cert-file", "", "path to the certificate PEM file")
	tlsSiteSetCmd.Flags().StringVar(&tlsSiteSetBinds.keyFile, "key-file", "", "path to the private key PEM file")
	tlsSiteSetCmd.Flags().StringVar(&tlsSiteSetBinds.cert, "cert", "", "certificate PEM text")
	tlsSiteSetCmd.Flags().StringVar(&tlsSiteSetBinds.key, "key", "", "private key PEM text")
	tlsSiteCmd.AddCommand(tlsSiteShowCmd, tlsSiteSetCmd)
	tlsCmd.AddCommand(tlsSiteCmd)
	rootCmd.AddCommand(tlsCmd)
}

// loadTLSPair resolves cert+key from --cert-file/--key-file or --cert/--key.
func loadTLSPair() (cert, key string, err error) {
	read := func(flagVal, file string) (string, error) {
		if file != "" {
			b, err := os.ReadFile(file)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", file, err)
			}
			return string(b), nil
		}
		if flagVal == "" {
			return "", errors.New("missing PEM input (use --cert/--key or --cert-file/--key-file)")
		}
		return flagVal, nil
	}
	cert, err = read(tlsAddBinds.cert, tlsAddBinds.certFile)
	if err != nil {
		return "", "", err
	}
	key, err = read(tlsAddBinds.key, tlsAddBinds.keyFile)
	if err != nil {
		return "", "", err
	}
	return cert, key, nil
}

func runTLSAdd(cmd *cobra.Command, args []string) error {
	host := args[0]
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cert, key, err := loadTLSPair()
	if err != nil {
		return err
	}

	st, cipher, dc, err := siteCertDeps(cmd, cfg, "")
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	defer func() { _ = dc.Close() }()

	fmt.Fprintf(cmd.OutOrStdout(), "Uploading certificate for %s …\n", host)
	svc := tlsService(cmd, st, cipher, dc, cfg)
	row, err := svc.ApplyHostCert(cmd.Context(), host, cert, key, !tlsAddBinds.noRefresh)
	if err != nil {
		return fmt.Errorf("apply cert: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✓ Stored certificate for %s (expires %s)\n",
		row.Domain, row.NotAfter.UTC().Format(time.RFC3339))
	fmt.Fprintf(cmd.OutOrStdout(), "  secrets: %s / %s\n", row.CertSecret, row.KeySecret)

	if tlsAddBinds.noRefresh {
		fmt.Fprintln(cmd.OutOrStdout(), "→ Skipping refresh (--no-refresh). Run `pmcluster cluster update` to apply.")
	}
	return nil
}

func runTLSList(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	st, _, _, err := siteCertDeps(cmd, cfg, "")
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	svc := tlsService(cmd, st, nil, nil, cfg)
	rows, err := svc.List(cmd.Context())
	if err != nil {
		return fmt.Errorf("list certificates: %w", err)
	}
	main, err := svc.MainDomain(cmd.Context())
	if err != nil {
		return fmt.Errorf("cluster domain: %w", err)
	}
	hosts := rows[:0]
	for _, r := range rows {
		if r.Domain == main {
			continue
		}
		hosts = append(hosts, r)
	}
	if len(hosts) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no per-host certificates stored — use `pmcluster tls hosts add <host>`.)")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tEXPIRES\tSANS\tCERT SECRET\tKEY SECRET")
	for _, h := range hosts {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", h.Domain, h.NotAfter.UTC().Format(time.RFC3339),
			strings.Join(h.SANs, ", "), h.CertSecret, h.KeySecret)
	}
	return w.Flush()
}

func runTLSRemove(cmd *cobra.Command, args []string) error {
	host := args[0]
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	st, cipher, dc, err := siteCertDeps(cmd, cfg, "")
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	defer func() { _ = dc.Close() }()

	err = tlsService(cmd, st, cipher, dc, cfg).RemoveHostCert(cmd.Context(), host, !tlsAddBinds.noRefresh)
	if err != nil {
		if errors.Is(err, store.ErrSiteCertNotFound) {
			return fmt.Errorf("no certificate stored for %q", host)
		}
		return fmt.Errorf("remove cert: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ Removed certificate for %s\n", host)
	if tlsAddBinds.noRefresh {
		fmt.Fprintln(cmd.OutOrStdout(), "→ Skipping refresh (--no-refresh). Run `pmcluster cluster update` to apply.")
	}
	return nil
}

// siteCertDeps builds the collaborators needed by the TLS commands.
func siteCertDeps(cmd *cobra.Command, cfg *config.Config, domain string) (*store.Store, *credentials.Cipher, docker.Client, error) {
	st, _, err := openStore()
	if err != nil {
		return nil, nil, nil, err
	}
	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		_ = st.Close()
		return nil, nil, nil, fmt.Errorf("open encryption key: %w", err)
	}
	dc, err := docker.New()
	if err != nil {
		_ = st.Close()
		return nil, nil, nil, fmt.Errorf("docker client: %w", err)
	}
	return st, cipher, dc, nil
}

// tlsService builds the local TLS service adapter used by the tls commands.
func tlsService(cmd *cobra.Command, st *store.Store, cipher *credentials.Cipher, dc docker.Client, cfg *config.Config) service.TLSService {
	return impl.NewTLS(st, cipher, dc, cluster.NewDockerCLIDeployer(cmd.OutOrStdout()),
		ooProvisioner(st, cipher, cmd.OutOrStdout(), cluster.PersistedDomain(cmd.Context(), st)),
		cfg.ConfigDir(), buildinfo.Version)
}

func runTLSSiteShow(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	st, _, _, err := siteCertDeps(cmd, cfg, "")
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	svc := tlsService(cmd, st, nil, nil, cfg)
	domain, err := svc.MainDomain(cmd.Context())
	if err != nil {
		return fmt.Errorf("cluster domain: %w", err)
	}
	if domain == "" {
		return errors.New("no persisted cluster domain found — run `pmcluster cluster up` first")
	}
	row, err := svc.GetSiteCert(cmd.Context(), domain)
	if err != nil {
		if errors.Is(err, store.ErrSiteCertNotFound) {
			fmt.Fprintf(cmd.OutOrStdout(), "No site certificate recorded yet for %s.\n", domain)
			fmt.Fprintf(cmd.OutOrStdout(), "Upload one with `pmcluster tls site set --cert-file cert.pem --key-file key.pem`.\n")
			return nil
		}
		return fmt.Errorf("get site cert: %w", err)
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Domain:\t%s\n", row.Domain)
	fmt.Fprintf(w, "Not before:\t%s\n", row.NotBefore.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "Expires:\t%s\n", row.NotAfter.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "SANs:\t%s\n", strings.Join(row.SANs, ", "))
	fmt.Fprintf(w, "Cert secret:\t%s\n", row.CertSecret)
	fmt.Fprintf(w, "Key secret:\t%s\n", row.KeySecret)
	fmt.Fprintf(w, "Cert hash:\t%s\n", row.CertHash)
	fmt.Fprintf(w, "Key hash:\t%s\n", row.KeyHash)
	fmt.Fprintf(w, "Updated:\t%s\n", row.UpdatedAt.UTC().Format(time.RFC3339))
	return w.Flush()
}

func runTLSSiteSet(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	st, cipher, dc, err := siteCertDeps(cmd, cfg, "")
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	defer func() { _ = dc.Close() }()

	svc := tlsService(cmd, st, cipher, dc, cfg)
	domain, err := svc.MainDomain(cmd.Context())
	if err != nil {
		return fmt.Errorf("cluster domain: %w", err)
	}
	if domain == "" {
		return errors.New("no persisted cluster domain found — run `pmcluster cluster up` first")
	}

	orig := tlsAddBinds
	tlsAddBinds = tlsAddFlags{certFile: tlsSiteSetBinds.certFile, keyFile: tlsSiteSetBinds.keyFile, cert: tlsSiteSetBinds.cert, key: tlsSiteSetBinds.key}
	cert, key, err := loadTLSPair()
	tlsAddBinds = orig
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Uploading certificate for %s …\n", domain)
	row, err := svc.SiteCert(cmd.Context(), domain, cert, key)
	if err != nil {
		return fmt.Errorf("apply site cert: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✓ Site certificate updated for %s (expires %s)\n",
		row.Domain, row.NotAfter.UTC().Format(time.RFC3339))
	fmt.Fprintf(cmd.OutOrStdout(), "  secrets: %s / %s\n", row.CertSecret, row.KeySecret)
	return nil
}
