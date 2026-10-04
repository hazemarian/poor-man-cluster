package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/workflow"
)

// backupRootDirDefault is the host-local archive every backup agent writes
// to. In a multi-node cluster each node has its own copy (the manager's is the
// "node 0" archive) — see docs/storage-and-databases.md. backupRootDir() lets
// PMCLUSTER_BACKUP_DIR relocate it: dev/test hosts (e.g. macOS) that cannot
// create /var/stack use the env var — the e2e swarm tier points it at a temp
// dir.
const backupRootDirDefault = "/var/stack/backup"

// backupRootDir returns the archive root, honoring PMCLUSTER_BACKUP_DIR.
func backupRootDir() string {
	if p := os.Getenv("PMCLUSTER_BACKUP_DIR"); p != "" {
		return p
	}
	return backupRootDirDefault
}

// BackupRootDir is the exported form of backupRootDir, for the stacks
// service (Move locates the newest whole-disk archive here).
func BackupRootDir() string { return backupRootDir() }

// effectiveVolumeRoot returns v when non-empty, else the bundled default
// (/var/stack/data). Render sites pass the volume_root setting through it so
// the platform stacks (and the backup agent's bind mounts) always follow the
// configured root.
func effectiveVolumeRoot(v string) string {
	if v == "" {
		return manifest.DefaultVolumeRoot
	}
	return v
}

// splitStorageNodes parses the storage_nodes cluster setting into a deduped
// hostname list (comma-separated, whitespace-trimmed). Kept in the cluster
// package — importing stacks would create an import cycle (stacks imports
// cluster).
func splitStorageNodes(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

// ensureStorageDirs creates the volume root and the backup archive dir on the
// host. Overridden in tests to keep them hermetic (t.TempDir).
var ensureStorageDirs = func(volumeRoot string) error {
	for _, dir := range []string{volumeRoot, backupRootDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create storage dir %s: %w", dir, err)
		}
	}
	return nil
}

// UpInput carries the bootstrap parameters for a fresh cluster install.
type UpInput struct {
	Domain string
	// Either CertPath+KeyPath OR ACMEEmail must be set; mutually exclusive.
	// CertPath/KeyPath: operator-provided PEM files loaded into Swarm secrets.
	// ACMEEmail: Traefik issues + renews via Let's Encrypt HTTP-01.
	// On a re-run these may be omitted to reuse the stored TLS state.
	CertPath              string
	KeyPath               string
	ACMEEmail             string
	ForceTLSMode          bool
	TraefikAdminUser      string
	OpenObserveAdminEmail string
	ConfigDir             string
	Version               string
	// VolumeRoot is the single host dir every container volume is forced
	// under (default manifest.DefaultVolumeRoot /var/stack/data).
	VolumeRoot string
	// SwarmAdvertiseAddr is passed to `docker swarm init` when this node is
	// not yet part of a Swarm (first-node bootstrap). Empty lets Docker (or
	// the CLI's detectNodeIP) pick the advertise address.
	SwarmAdvertiseAddr string
}

// UpResult includes plaintext passwords for any credentials newly minted
// on this run; existing ones are returned with NewlyCreated=false.
type UpResult struct {
	NewNetworks          []string
	NewSecrets           []string
	NewConfigs           []string
	StacksDeployed       []string
	BootstrapCredentials map[string]*ManagedCredential
}

// UpDeps are the collaborators Up needs: the control-plane store, the
// credential cipher, the runtime client, the stack deployer and a sink for
// workflow output.
type UpDeps struct {
	Store    *store.Store
	Cipher   *credentials.Cipher
	Docker   runtime.Client
	Deployer StackDeployer
	Stdout   io.Writer
}

// Up brings the cluster up end-to-end as a sequence of named steps: preflight
// → overlay networks → TLS secrets → bootstrap credentials → rendered configs
// → stack deploys (infra → edge → observability → backup) → health wait →
// install state persistence.
//
// OpenObserve runs with the admin email:password (ZO_ROOT_USER_EMAIL /
// ZO_ROOT_USER_PASSWORD from the openobserve_admin credential + the
// zo_root_user_password Swarm secret). The OTel collector and the Traefik
// openobserve-auto-auth middleware use the SAME admin credentials — no
// provisioning API calls, no automation user, no ingestion token.
func Up(ctx context.Context, deps UpDeps, in UpInput) (*UpResult, error) {

	state, err := loadTLSSettings(ctx, deps.Store)
	if err != nil {
		return nil, err
	}
	merged, err := mergeTLSState(in, state)
	if err != nil {
		return nil, err
	}
	in = merged

	if err := validateUpInput(in); err != nil {
		return nil, err
	}

	// cluster up is init-only: it must never touch a cluster that is already
	// running. A persisted domain or TLS state means `cluster update` owns the
	// cluster from here on.
	if deps.Store != nil {
		installed, err := clusterInstalled(ctx, deps.Store)
		if err != nil {
			return nil, err
		}
		if installed {
			return nil, fmt.Errorf("cluster already initialised — run `pmcluster cluster update` instead (cluster up is for fresh installs only)")
		}
	}

	out := io.Discard
	if deps.Stdout != nil {
		out = deps.Stdout
	}

	res := &UpResult{}
	var (
		certSecret, keySecret string
		creds                 map[string]*ManagedCredential
		render                RenderInput
		sso                   ssoState
		ssoSecret             string
	)

	wf := workflow.NewWorkflow(out)
	wf.Add("Preflight: Docker reachable, Swarm active, this node is a manager", func(ctx context.Context) error {
		return Preflight(ctx, deps.Docker)
	})
	wf.Add("Ensuring storage root directories (/var/stack/data, /var/stack/backup)", func(ctx context.Context) error {
		root := in.VolumeRoot
		if root == "" {
			root = manifest.DefaultVolumeRoot
		}
		if err := ensureStorageDirs(root); err != nil {
			return err
		}
		fmt.Fprintf(out, "  ✓ %s ready\n", root)
		fmt.Fprintf(out, "  ✓ %s ready\n", backupRootDir())
		return nil
	})
	wf.Add("Syncing platform config templates into the store", func(ctx context.Context) error {
		syncRes, err := SyncPlatformConfigs(ctx, deps.Store, in.Version)
		if err != nil {
			return err
		}
		for _, n := range syncRes.Created {
			fmt.Fprintf(out, "  ✓ %s recorded in store\n", n)
		}
		for _, n := range syncRes.Updated {
			fmt.Fprintf(out, "  ✓ %s updated in store\n", n)
		}
		return nil
	})
	wf.Add("Ensuring overlay networks", func(ctx context.Context) error {
		created, err := EnsureBundledNetworks(ctx, deps.Docker)
		if err != nil {
			return fmt.Errorf("ensure networks: %w", err)
		}
		res.NewNetworks = created
		return nil
	})
	// Default the storage node BEFORE the platform stacks are rendered, so the
	// backup agent is rendered in its final form (global, constrained to
	// storage nodes) from the very first deploy. Defaulting it in
	// persistInstallState (after deploy) made the first `cluster update`
	// switch backup_volume-backup from replicated to global — a mode change
	// Docker rejects in place ("service mode change is not allowed").
	wf.Add("Defaulting storage node", func(ctx context.Context) error {
		return defaultStorageNode(ctx, deps)
	})
	wf.Add("TLS certificate (Let's Encrypt or operator cert/key)", func(ctx context.Context) error {
		if in.ACMEEmail != "" {
			fmt.Fprintf(out, "  ▶ TLS via Let's Encrypt (Traefik HTTP-01) — port 80 must be reachable from the internet\n")
			return nil
		}
		var certCreated, keyCreated bool
		certSecret, certCreated, err = EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "cert", in.CertPath)
		if err != nil {
			return fmt.Errorf("ensure cert secret: %w", err)
		}
		keySecret, keyCreated, err = EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "key", in.KeyPath)
		if err != nil {
			return fmt.Errorf("ensure key secret: %w", err)
		}
		if certCreated || keyCreated {
			res.NewSecrets = append(res.NewSecrets, certSecret, keySecret)
		}
		return nil
	})
	wf.Add("Bootstrapping managed credentials (Traefik / OpenObserve / edge)", func(ctx context.Context) error {
		credMgr := &CredentialsManager{
			Store:  deps.Store,
			Cipher: deps.Cipher,
			Docker: deps.Docker,
		}
		creds, err = credMgr.Bootstrap(ctx, BootstrapInput{
			TraefikAdminUser:      in.TraefikAdminUser,
			OpenObserveAdminEmail: in.OpenObserveAdminEmail,
		})
		if err != nil {
			return fmt.Errorf("bootstrap credentials: %w", err)
		}
		res.BootstrapCredentials = creds
		for name, c := range creds {
			switch {
			case c.NewlyCreated && c.SwarmSecretCreated:
				res.NewSecrets = append(res.NewSecrets, c.SwarmSecretName)
				fmt.Fprintf(out, "  ✓ %-20s newly created (swarm secret %s)\n", name, c.SwarmSecretName)
			case c.NewlyCreated && !c.SwarmSecretCreated:
				fmt.Fprintf(out, "  ⚠ %-20s store updated, but Swarm secret %s pre-existed (passwords may diverge)\n", name, c.SwarmSecretName)
			case !c.NewlyCreated && c.SwarmSecretCreated:
				res.NewSecrets = append(res.NewSecrets, c.SwarmSecretName)
				fmt.Fprintf(out, "  ✓ %-20s already in store; Swarm secret %s recreated\n", name, c.SwarmSecretName)
			default:
				if c.UsernameChanged {
					fmt.Fprintf(out, "  ✓ %-20s username updated to %s\n", name, c.Username)
				} else {
					fmt.Fprintf(out, "  ✓ %-20s already present\n", name)
				}
			}
		}
		if creds["edge_admin"] != nil {
			fmt.Fprintf(out, "  ✔ edge console credentials minted — retrieve the admin password with `pmcluster credentials show edge_admin`\n")
		}
		if obsCred := creds["openobserve_admin"]; obsCred != nil && obsCred.UsernameChanged {
			fmt.Fprintf(out, "  ⚠ OpenObserve email changed → resetting data volume so new credentials take effect\n")
			if err := deps.Docker.VolumeRemove(ctx, "observability_openobserve_data"); err != nil {
				return fmt.Errorf("reset openobserve data volume: %w (manual: docker volume rm observability_openobserve_data)", err)
			}
		}
		return nil
	})
	wf.Add("Rendering and creating Docker configs (OTel pipeline, Traefik dynamic)", func(ctx context.Context) error {
		openobsCred := creds["openobserve_admin"]
		if openobsCred == nil {
			return fmt.Errorf("internal: openobserve_admin credential missing after bootstrap")
		}

		sso, err = loadSSOSettings(ctx, deps.Store)
		if err != nil {
			return err
		}
		if err := sso.validate(); err != nil {
			return err
		}
		ssoSecret = ""
		if sso.Enabled {
			// sso_cookie_secret is minted by the credentials bootstrap; pull
			// its plaintext so oauth2-proxy gets a stable cookie secret.
			cookieCred, ok := creds["sso_cookie_secret"]
			if !ok || cookieCred == nil {
				return fmt.Errorf("internal: sso_cookie_secret credential missing after bootstrap (SSO enabled)")
			}
			ssoSecret = cookieCred.Password
		}

		hostCerts, err := loadHostCertEntries(ctx, deps.Store, in.Domain)
		if err != nil {
			return err
		}

		ooL, ooM, ooT := loadOORetention(ctx, deps.Store)
		s3b := loadBackupS3(ctx, deps.Store)
		render = RenderInput{
			Domain:                   in.Domain,
			OpenObserveAdminEmail:    openobsCred.Username,
			OpenObserveAdminPassword: openobsCred.Password,
			OpenObserveBasicAuth:     openObserveBasicAuth(openobsCred.Username, openobsCred.Password),
			OpenObserveSessionCookie: openObserveSessionCookie(openobsCred.Username, openobsCred.Password),
			ACMEEmail:                in.ACMEEmail,
			ConfigDir:                in.ConfigDir,
			ConfigStore:              deps.Store,
			DataDir:                  filepath.Dir(in.ConfigDir),
			VolumeRoot:               effectiveVolumeRoot(in.VolumeRoot),
			BackupDir:                backupRootDir(),
			HostCerts:                hostCerts,
			CertSecretName:           certSecret,
			KeySecretName:            keySecret,
			EdgeImage:                EdgeImageFor(),
			EdgeLoginDisabled:        loadEdgeLoginDisabled(ctx, deps.Store),
			BackupAllNodes:           loadBackupAllNodes(ctx, deps.Store),
			BackupRetentionDays:      LoadBackupRetentionDays(ctx, deps.Store),
			StorageNodeConstraint:    deps.Store.GetSettingDefault(ctx, SettingStorageNodes(), "") != "",
			StorageNodeLabel:         runtime.StorageNodeLabel,
			PlatformNode:             loadPlatformNode(ctx, deps.Store),
			OOLogsRetentionDays:      ooL, OOMetricsRetentionDays: ooM, OOTracesRetentionDays: ooT,
			BackupS3:        s3b,
			SSOEnabled:      sso.Enabled,
			SSOCookieSecret: ssoSecret,
			SSOClientID:     sso.ClientID,
			SSOClientSecret: sso.ClientSecret,
			SSOGitHubOrg:    sso.GitHubOrg,
			SSOGitHubRepos:  sso.GitHubRepos,
			SSOCookieExpire: sso.CookieExpire,
		}

		renderedConfigs, err := EnsureRenderedConfigs(ctx, deps.Docker, &render, in.Version)
		if err != nil {
			return err
		}
		for _, oc := range renderedConfigs {
			if oc.Created {
				res.NewConfigs = append(res.NewConfigs, oc.Name)
			}
		}

		edgeName, edgeCreated, err := ensureEdgeConfig(ctx, deps.Docker, in.Version, render)
		if err != nil {
			return err
		}
		if edgeCreated {
			res.NewConfigs = append(res.NewConfigs, edgeName)
		}
		return nil
	})
	wf.Add("Deploying stacks (infra → edge → observability → backup)", func(ctx context.Context) error {
		stacks := []stackName{StackInfra, StackEdge, StackObservability, StackBackup}
		if sso.Enabled {
			// sso (oauth2-proxy) is deployed alongside the platform stacks so
			// Traefik's forwardAuth middleware has a live backend.
			stacks = append(stacks, StackSSO)
		}
		for _, s := range stacks {
			if err := deployStack(ctx, out, deps.Deployer, s, render); err != nil {
				return err
			}
			res.StacksDeployed = append(res.StacksDeployed, string(s))
		}
		return nil
	})
	wf.Add("Waiting for all services to become healthy", func(ctx context.Context) error {
		if err := WaitHealthyStacks(ctx, deps.Docker, out); err != nil {
			return fmt.Errorf("health check: %w", err)
		}
		return nil
	})
	wf.Add("Snapshotting rendered configs into the store", func(ctx context.Context) error {
		if deps.Store == nil {
			return nil
		}
		rendered, err := renderPlatformConfigs(render)
		if err != nil {
			return fmt.Errorf("render platform configs for persistence: %w", err)
		}
		for name, content := range rendered {
			if err := deps.Store.SetRendered(ctx, name, string(content)); err != nil && !errors.Is(err, store.ErrConfigNotFound) {
				return fmt.Errorf("store rendered config %s: %w", name, err)
			}
		}
		return nil
	})
	wf.Add("Persisting install state", func(ctx context.Context) error {
		return persistInstallState(ctx, deps, in)
	})
	wf.Add("Cluster up complete.", func(ctx context.Context) error {
		return nil
	})

	if err := wf.Run(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// defaultStorageNode records the leader node as the default storage node when
// no storage_nodes list is configured yet — a stateful stack with no explicit
// placement pins to it, so its data has a home from day one. The list is
// hostname-based and role-agnostic: later nodes join as storage nodes
// explicitly (pmcluster join --storage-node) and round-robin placement spreads
// stateful stacks across them. The default storage node also carries the
// pmcluster.storage label so the backup agent (constrained to storage nodes)
// runs exactly where app data lives. Idempotent: no-op once a list is set.
func defaultStorageNode(ctx context.Context, deps UpDeps) error {
	if deps.Store == nil {
		return nil
	}
	if cur := deps.Store.GetSettingDefault(ctx, SettingStorageNodes(), ""); cur == "" && deps.Docker != nil {
		if nodes, err := deps.Docker.NodeList(ctx); err == nil {
			for _, n := range nodes {
				if n.IsLeader && n.Hostname != "" {
					if err := deps.Store.SetSetting(ctx, SettingStorageNodes(), n.Hostname); err != nil {
						return fmt.Errorf("persist %s: %w", SettingStorageNodes(), err)
					}
					if lerr := deps.Docker.SetNodeLabel(ctx, n.ID, runtime.StorageNodeLabel, "true"); lerr != nil {
						return fmt.Errorf("label storage node %s: %w", n.Hostname, lerr)
					}
					break
				}
			}
		}
	}
	return nil
}

// persistInstallState records the install inputs used on this (possibly
// idempotent) bring-up: TLS mode + cert/key paths, domain, and OO admin email.
func persistInstallState(ctx context.Context, deps UpDeps, in UpInput) error {
	if deps.Store == nil {
		return nil
	}
	state := tlsState{
		Mode:      requestedTLSMode(in),
		CertPath:  in.CertPath,
		KeyPath:   in.KeyPath,
		ACMEEmail: in.ACMEEmail,
	}
	if err := state.save(ctx, deps.Store); err != nil {
		return fmt.Errorf("persist tls state: %w", err)
	}
	for k, v := range map[string]string{
		settingDomain:  in.Domain,
		settingOOEmail: in.OpenObserveAdminEmail,
	} {
		if err := deps.Store.SetSetting(ctx, k, v); err != nil {
			return fmt.Errorf("persist %s: %w", k, err)
		}
	}
	// Storage-node defaulting happens earlier (Defaulting storage node step)
	// so platform stacks render in final form before deploy; calling the
	// helper here keeps the persisted state authoritative on re-runs.
	if err := defaultStorageNode(ctx, deps); err != nil {
		return err
	}
	return nil
}

func validateUpInput(in UpInput) error {
	if in.Domain == "" {
		return fmt.Errorf("--domain is required")
	}
	if in.OpenObserveAdminEmail == "" {
		return fmt.Errorf("--openobserve-email is required (used as OpenObserve admin login)")
	}
	hasBYOTLS := in.CertPath != "" || in.KeyPath != ""
	hasACME := in.ACMEEmail != ""
	if hasBYOTLS && hasACME {
		return fmt.Errorf("choose ONE TLS mode: --acme-email (Let's Encrypt) OR --cert + --key (operator-supplied)")
	}
	if !hasBYOTLS && !hasACME {
		return fmt.Errorf("a TLS mode is required: --acme-email <you@host> (Let's Encrypt) OR --cert <pem> --key <pem>")
	}
	if hasBYOTLS && (in.CertPath == "" || in.KeyPath == "") {
		return fmt.Errorf("--cert and --key must both be provided")
	}
	return nil
}
