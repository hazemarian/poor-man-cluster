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
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster/tlscerts"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
)

// tlsCmd groups the per-host TLS certificate commands.
var tlsCmd = &cobra.Command{
	Use:   "tls",
	Short: "Manage per-host TLS certificates served by Traefik",
	Long: `A customer's own domain can serve the same app that lives on a
pmcluster subdomain, on its own origin with its own real certificate. These
commands store per-host certs (as text) under ~/.pmcluster/config/hosts/<host>/
and refresh Traefik so it serves them for the matching SNI.

The cluster's own default wildcard cert (set via 'pmcluster cluster up
--cert/--key') is never touched by these commands.`,
}

var tlsHostsCmd = &cobra.Command{
	Use:   "hosts",
	Short: "Per-host TLS certificate store",
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
	Long: `Stores cert+key for <host> under ~/.pmcluster/config/hosts/<host>/.

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

	mgr := tlscerts.New(cfg.ConfigDir())
	hc, err := mgr.Put(cmd.Context(), host, cert, key)
	if err != nil {
		return fmt.Errorf("store cert: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✓ Stored certificate for %s (expires %s)\n",
		hc.Host, hc.NotAfter.Format(time.RFC3339))

	if !tlsAddBinds.noRefresh {
		created, err := refreshHostCerts(cmd, cfg)
		if err != nil {
			return err
		}
		if created {
			fmt.Fprintln(cmd.OutOrStdout(), "✓ Traefik config updated and infra stack re-deployed")
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "✓ Traefik config already up to date (no change detected)")
		}
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "→ Skipping refresh (--no-refresh). Run `pmcluster cluster update` to apply.")
	}
	return nil
}

func runTLSList(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	mgr := tlscerts.New(cfg.ConfigDir())
	hosts, err := mgr.List(cmd.Context())
	if err != nil {
		return fmt.Errorf("list host certs: %w", err)
	}
	if len(hosts) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no per-host certificates stored — use `pmcluster tls hosts add <host>`.)")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tEXPIRES\tSANS")
	for _, h := range hosts {
		fmt.Fprintf(w, "%s\t%s\t%s\n", h.Host, h.NotAfter.Format(time.RFC3339), strings.Join(h.SANs, ", "))
	}
	return w.Flush()
}

func runTLSRemove(cmd *cobra.Command, args []string) error {
	host := args[0]
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	mgr := tlscerts.New(cfg.ConfigDir())
	removed, err := mgr.Remove(cmd.Context(), host)
	if err != nil {
		return fmt.Errorf("remove cert: %w", err)
	}
	if !removed {
		return fmt.Errorf("no certificate stored for %q", host)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ Removed certificate for %s\n", host)

	if !tlsAddBinds.noRefresh {
		created, err := refreshHostCerts(cmd, cfg)
		if err != nil {
			return err
		}
		if created {
			fmt.Fprintln(cmd.OutOrStdout(), "✓ Traefik config updated and infra stack re-deployed")
		}
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "→ Skipping refresh (--no-refresh). Run `pmcluster cluster update` to apply.")
	}
	return nil
}

// refreshHostCerts re-renders Traefik (including per-host certs) and re-deploys
// the infra stack when the dynamic config changed.
func refreshHostCerts(cmd *cobra.Command, cfg *config.Config) (bool, error) {
	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return false, fmt.Errorf("open encryption key: %w", err)
	}
	dc, err := docker.New()
	if err != nil {
		return false, fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dc.Close() }()

	st, _, err := openStore()
	if err != nil {
		return false, err
	}
	defer func() { _ = st.Close() }()

	return cluster.RefreshHostCerts(cmd.Context(), cluster.HostCertsDeps{
		Store:    st,
		Cipher:   cipher,
		Docker:   dc,
		Deployer: cluster.NewDockerCLIDeployer(cmd.OutOrStdout()),
	}, cfg.ConfigDir(), buildinfo.Version)
}
