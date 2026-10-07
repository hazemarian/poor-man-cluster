package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/workflow"
)

// UpdateInput carries the config-dir + build version for `cluster update`.
// Unlike Up, UpdateInput carries no TLS/domain/credential fields — those are
// read back from the persisted install state so update needs no re-bootstrap.
type UpdateInput struct {
	ConfigDir string
	Version   string

	// Force makes Update existence-unaware AND hash-unaware: every platform
	// stack is redeployed regardless of its stored rendered hash or whether it
	// is already present in the Swarm. Used by `cluster reset` (non-destructive
	// rebuild of the Swarm side FROM the DB). Networks, credentials, secrets and
	// configs are still re-ensured idempotently.
	Force bool
}

// UpdateResult reports which configs/secrets were rotated and which stacks
// were re-deployed on this update. A no-op update (nothing changed) returns
// empty/zero results and deploys nothing.
type UpdateResult struct {
	OTelConfig     string
	OTelCreated    bool
	TraefikConfig  string
	TraefikCreated bool
	CertSecret     string
	KeySecret      string
	CertCreated    bool
	KeyCreated     bool
	EdgeConfig     string
	EdgeCreated    bool
	EdgeDeployed   bool
	StacksDeployed []string
}

// UpdateDeps are the collaborators Update needs to reconcile an existing
// cluster: store, cipher, runtime client, deployer and output sink.
type UpdateDeps struct {
	Store    *store.Store
	Cipher   *credentials.Cipher
	Docker   runtime.Client
	Deployer StackDeployer
	Stdout   io.Writer
}

// Update re-provisions the OTel collector + Traefik dynamic configs and the
// TLS cert/key, pushing only what changed into Docker, and re-deploying only
// the stack(s) whose rendered content moved.
//
// It first syncs the platform config templates into the store to the current
// build version — the DB is the single source of truth, so stale rows are
// refreshed from the embedded defaults and console edits at the current
// version are preserved. It never re-bootstraps credentials and never resets
// volumes. It is content-aware end to end: each stack's freshly rendered
// compose is hashed and compared against the stored rendered_hash, so an
// unchanged stack is not redeployed (a no-op update is just a report).
func Update(ctx context.Context, deps UpdateDeps, in UpdateInput) (*UpdateResult, error) {
	out := io.Discard
	if deps.Stdout != nil {
		out = deps.Stdout
	}
	res := &UpdateResult{}

	var (
		state     tlsState
		domain    string
		render    RenderInput
		sso       ssoState
		ssoSecret string

		// swarmChanged is set when the live Swarm cluster ID differs from the
		// stored one (or is unset) — a wiped + re-initialised Swarm. It forces
		// a full redeploy of every platform stack this run.
		swarmChanged bool
	)

	wf := workflow.NewWorkflow(out)
	wf.Add("Preflight: Docker reachable, Swarm active", func(ctx context.Context) error {
		return Preflight(ctx, deps.Docker)
	})

	wf.Add("Detecting swarm identity (wipe detection)", func(ctx context.Context) error {
		if deps.Docker == nil || deps.Store == nil {
			return nil
		}
		live, err := deps.Docker.SwarmID(ctx)
		if err != nil {
			// Best-effort: an unreadable swarm ID is not fatal — it only means
			// we cannot detect a wipe this run.
			fmt.Fprintf(out, "  ⚠ could not read swarm ID (%v) — skipping wipe detection\n", err)
			return nil
		}
		stored := deps.Store.GetSettingDefault(ctx, settingSwarmID, "")
		if live != "" && live != stored {
			swarmChanged = true
			fmt.Fprintf(out, "  ▶ swarm identity changed (%s → %s) — forcing full redeploy\n", stored, live)
			if err := deps.Store.SetSetting(ctx, settingSwarmID, live); err != nil {
				return fmt.Errorf("persist swarm_id: %w", err)
			}
		}
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

	wf.Add("Loading persisted install state (domain, TLS, credentials)", func(ctx context.Context) error {
		if deps.Store == nil {
			return fmt.Errorf("update requires a store (config_dir must be initialised via `pmcluster init`)")
		}
		var err error
		state, err = loadTLSSettings(ctx, deps.Store)
		if err != nil {
			return err
		}
		domain = deps.Store.GetSettingDefault(ctx, settingDomain, "")
		if domain == "" {
			return fmt.Errorf("no persisted domain found — run `cluster up` before `cluster update`")
		}
		return nil
	})

	wf.Add("Loading SSO settings + cookie secret", func(ctx context.Context) error {
		var err error
		sso, err = loadSSOSettings(ctx, deps.Store)
		if err != nil {
			return err
		}
		if err := sso.validate(); err != nil {
			return err
		}
		ssoSecret = ""
		if sso.Enabled {
			cookieCred, err := deps.Store.GetCredential(ctx, "sso_cookie_secret")
			switch {
			case errors.Is(err, store.ErrCredentialNotFound):
				// SSO enabled on a cluster whose bootstrap predates the
				// credential — self-heal by minting it now (idempotent).
				mgr := &CredentialsManager{Store: deps.Store, Cipher: deps.Cipher, Docker: deps.Docker, Deployer: deps.Deployer}
				mc, cerr := mgr.Ensure(ctx, "sso_cookie_secret")
				if cerr != nil {
					return fmt.Errorf("bootstrap sso_cookie_secret (SSO enabled): %w", cerr)
				}
				ssoSecret = mc.Password
			case err != nil:
				return fmt.Errorf("load sso_cookie_secret credential (SSO enabled): %w", err)
			default:
				plain, derr := deps.Cipher.Decrypt(cookieCred.PasswordCiphertext)
				if derr != nil {
					return fmt.Errorf("decrypt sso_cookie_secret: %w", derr)
				}
				ssoSecret = string(plain)
			}
		}
		return nil
	})

	wf.Add("Ensuring the in-cluster SeaweedFS backup credential", func(ctx context.Context) error {
		if _, err := deps.Store.GetCredential(ctx, "seaweedfs_admin"); errors.Is(err, store.ErrCredentialNotFound) {
			// Mint the SeaweedFS S3 credential on clusters whose bootstrap
			// predates the in-cluster backup store, so this render enables it
			// (idempotent; the swarm secret is created alongside).
			mgr := &CredentialsManager{Store: deps.Store, Cipher: deps.Cipher, Docker: deps.Docker, Deployer: deps.Deployer}
			if _, cerr := mgr.Ensure(ctx, "seaweedfs_admin"); cerr != nil {
				return fmt.Errorf("bootstrap seaweedfs_admin: %w", cerr)
			}
		} else if err != nil {
			return fmt.Errorf("load seaweedfs_admin credential: %w", err)
		}
		return nil
	})

	wf.Add("Loading OpenObserve credentials", func(ctx context.Context) error {
		ooCred, err := deps.Store.GetCredential(ctx, "openobserve_admin")
		if err != nil {
			return fmt.Errorf("load openobserve_admin credential (run `cluster up` first): %w", err)
		}
		ooPass, err := deps.Cipher.Decrypt(ooCred.PasswordCiphertext)
		if err != nil {
			return fmt.Errorf("decrypt openobserve_admin password: %w", err)
		}
		ooL, ooM, ooT := loadOORetention(ctx, deps.Store)
		s3b := loadBackupS3(ctx, deps.Store)
		objStore, objStoreErr := loadObjectStore(ctx, deps.Docker, deps.Store, deps.Cipher)
		if objStoreErr != nil {
			return objStoreErr
		}
		render = RenderInput{
			Domain:                   domain,
			OpenObserveAdminEmail:    ooCred.Username,
			OpenObserveAdminPassword: string(ooPass),
			OpenObserveBasicAuth:     openObserveBasicAuth(ooCred.Username, string(ooPass)),
			OpenObserveSessionCookie: openObserveSessionCookie(ooCred.Username, string(ooPass)),
			ACMEEmail:                state.ACMEEmail,
			ConfigDir:                in.ConfigDir,
			ConfigStore:              deps.Store,
			DataDir:                  filepath.Dir(in.ConfigDir),
			VolumeRoot:               effectiveVolumeRoot(deps.Store.GetSettingDefault(ctx, SettingVolumeRoot(), "")),
			BackupDir:                backupRootDir(),
			EdgeImage:                EdgeImageFor(),
			EdgeLoginDisabled:        loadEdgeLoginDisabled(ctx, deps.Store),
			BackupAllNodes:           loadBackupAllNodes(ctx, deps.Store),
			BackupRetentionDays:      LoadBackupRetentionDays(ctx, deps.Store),
			BackupCron:               LoadBackupCron(ctx, deps.Store),
			StorageNodeConstraint:    deps.Store.GetSettingDefault(ctx, SettingStorageNodes(), "") != "",
			StorageNodeLabel:         runtime.StorageNodeLabel,
			PlatformNode:             loadPlatformNode(ctx, deps.Store),
			OOLogsRetentionDays:      ooL, OOMetricsRetentionDays: ooM, OOTracesRetentionDays: ooT,
			BackupS3:        s3b,
			Store:           objStore,
			SSOEnabled:      sso.Enabled,
			SSOCookieSecret: ssoSecret,
			SSOClientID:     sso.ClientID,
			SSOClientSecret: sso.ClientSecret,
			SSOGitHubOrg:    sso.GitHubOrg,
			SSOGitHubRepos:  sso.GitHubRepos,
			SSOCookieExpire: sso.CookieExpire,
		}
		hostCerts, err := loadHostCertEntries(ctx, deps.Store, domain)
		if err != nil {
			return err
		}
		render.HostCerts = hostCerts
		return nil
	})

	wf.Add("Ensuring storage root directories exist", func(ctx context.Context) error {
		// BUG-002 fix: `cluster up` created these dirs for the volume root in
		// effect at that time; a post-up volume_root change leaves the new
		// root missing and bind mounts get rejected ("bind source path does
		// not exist"). Re-create them on every update so operators can move
		// the storage root without manual mkdir + service --force.
		if err := ensureStorageDirs(render.VolumeRoot); err != nil {
			return err
		}
		if err := ensureStorageDirs(backupRootDir()); err != nil {
			return err
		}
		return nil
	})

	wf.Add("Ensuring overlay networks", func(ctx context.Context) error {
		// External overlay networks (traefik-net, monitoring-net) are created
		// only by `cluster up`; a wiped Swarm loses them even though the store
		// survived. Re-ensure them here so a self-healing update can recreate
		// them before any stack deploy references them.
		if deps.Docker == nil {
			return nil
		}
		created, err := EnsureBundledNetworks(ctx, deps.Docker)
		if err != nil {
			return fmt.Errorf("ensure networks: %w", err)
		}
		for _, n := range created {
			fmt.Fprintf(out, "  ✓ network %s created\n", n)
		}
		return nil
	})

	wf.Add("Ensuring managed credentials + swarm secrets", func(ctx context.Context) error {
		// Re-materialize every managed credential's Swarm secret from the DB
		// ciphertext before the stack reconcile. A wiped Swarm loses the
		// Raft-replicated secrets even though the store still holds the
		// ciphertext — deploy would otherwise fail with "secret not found".
		// EnsureMaterialized only re-creates a MISSING secret; it never rotates
		// or changes an existing value (edge_api_token included).
		if deps.Docker == nil {
			return nil
		}
		mgr := &CredentialsManager{Store: deps.Store, Cipher: deps.Cipher, Docker: deps.Docker, Deployer: deps.Deployer}
		for _, spec := range bootstrapSpecs() {
			created, err := mgr.EnsureMaterialized(ctx, spec.name)
			if err != nil {
				return fmt.Errorf("materialize %s: %w", spec.name, err)
			}
			if created {
				fmt.Fprintf(out, "  ✓ re-materialized swarm secret %s (from DB ciphertext)\n", spec.swarmSecretName)
			}
		}
		// Stale-era credential from the pre-SeaweedFS MinIO store. Harmless if
		// absent; re-materialize its secret if the row survived the wipe.
		if created, err := ensureLegacyPlainSecret(ctx, deps, "minio_root_password", "minio_root_password"); err != nil {
			return fmt.Errorf("materialize minio_root_password: %w", err)
		} else if created {
			fmt.Fprintf(out, "  ✓ re-materialized swarm secret minio_root_password\n")
		}
		return nil
	})

	wf.Add("Repairing swarm configs/secrets from the DB index (rebuild-on-missing)", func(ctx context.Context) error {
		if deps.Docker == nil {
			return nil
		}
		// Configs: every DB row is materialized into a content-addressed
		// swarm config (<name>_<sha8>). If the object is missing (swarm lost
		// it, or it was created before the swarm-first era) it is rebuilt
		// from the DB value — the DB is the index AND holds the value.
		cfgs, err := deps.Store.ListConfigs(ctx, "", "")
		if err != nil {
			return fmt.Errorf("list configs for swarm repair: %w", err)
		}
		repairedConfigs := 0
		for _, c := range cfgs {
			target := store.SwarmConfigName(c.Name, c.Hash)
			exists, err := deps.Docker.ConfigExists(ctx, target)
			if err != nil {
				fmt.Fprintf(out, "  ⚠ config %s: check swarm object: %v\n", c.Name, err)
				continue
			}
			if exists {
				continue
			}
			if err := deps.Docker.ConfigCreate(ctx, runtime.ConfigSpec{
				Name: target,
				Data: []byte(c.Content),
				Labels: map[string]string{
					pmclusterLabel:        "true",
					"pmcluster.base":      c.Name,
					"pmcluster.data_hash": c.Hash,
				},
			}); err != nil {
				fmt.Fprintf(out, "  ⚠ config %s: rebuild swarm object %s: %v\n", c.Name, target, err)
				continue
			}
			repairedConfigs++
		}
		if repairedConfigs > 0 {
			fmt.Fprintf(out, "  ▶ rebuilt %d missing swarm config(s) from the DB index\n", repairedConfigs)
		}
		// Secrets: same rule — every DB row whose swarm secret is missing is
		// rebuilt from the stored (encrypted-at-rest, plaintext-recovered by
		// the caller) value. Secrets are Raft-replicated too, so a lost
		// leader must not lose a secret.
		secs, err := deps.Store.ListSecrets(ctx, "", "")
		if err != nil {
			return fmt.Errorf("list secrets for swarm repair: %w", err)
		}
		repairedSecrets := 0
		for _, s := range secs {
			target := store.SwarmSecretName(s.Name, s.Hash)
			exists, err := deps.Docker.SecretExists(ctx, target)
			if err != nil {
				fmt.Fprintf(out, "  ⚠ secret %s: check swarm object: %v\n", s.Name, err)
				continue
			}
			if exists {
				continue
			}
			// The store's payload column is the encrypted-at-rest ciphertext;
			// rebuilds can only happen when the row carries the plaintext
			// fingerprint hash (it does) and the value was already mirrored
			// once. If the ciphertext cannot be recovered, the operator must
			// recreate the secret — warn, don't fail the update.
			plain, err := deps.Cipher.Decrypt(s.Payload)
			if err != nil {
				fmt.Fprintf(out, "  ⚠ secret %s: cannot rebuild swarm object (ciphertext not recoverable: %v)\n", s.Name, err)
				continue
			}
			if err := deps.Docker.SecretCreate(ctx, runtime.SecretSpec{
				Name: target,
				Data: []byte(plain),
				Labels: map[string]string{
					pmclusterLabel:        "true",
					"pmcluster.base":      s.Name,
					"pmcluster.data_hash": s.Hash,
				},
			}); err != nil {
				fmt.Fprintf(out, "  ⚠ secret %s: rebuild swarm object %s: %v\n", s.Name, target, err)
				continue
			}
			repairedSecrets++
		}
		if repairedSecrets > 0 {
			fmt.Fprintf(out, "  ▶ rebuilt %d missing swarm secret(s) from the DB index\n", repairedSecrets)
		}
		return nil
	})

	wf.Add("TLS certificate (ACME or stored cert/key)", func(ctx context.Context) error {
		if render.ACMEEmail != "" {
			return nil
		}
		if state.CertPath == "" || state.KeyPath == "" {
			return fmt.Errorf("TLS is not ACME (no ACME email) but no cert/key paths are persisted — run `cluster up` with --cert/--key (or --acme-email) before `cluster update`")
		}
		certName, certCreated, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "cert", state.CertPath)
		if err != nil {
			return fmt.Errorf("ensure cert secret: %w", err)
		}
		keyName, keyCreated, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "key", state.KeyPath)
		if err != nil {
			return fmt.Errorf("ensure key secret: %w", err)
		}
		res.CertSecret, res.KeySecret = certName, keyName
		res.CertCreated, res.KeyCreated = certCreated, keyCreated
		render.CertSecretName, render.KeySecretName = certName, keyName
		warnSiteCertExpiry(ctx, deps.Store, domain, deps.Stdout)
		return nil
	})

	wf.Add("Rendering OTel + Traefik + edge configs (content-aware)", func(ctx context.Context) error {
		rendered, err := EnsureRenderedConfigs(ctx, deps.Docker, &render, in.Version)
		if err != nil {
			return err
		}
		res.OTelConfig = rendered["pmcluster_otel_config"].Name
		res.OTelCreated = rendered["pmcluster_otel_config"].Created
		res.TraefikConfig = rendered["pmcluster_traefik_dynamic"].Name
		res.TraefikCreated = rendered["pmcluster_traefik_dynamic"].Created

		edgeName, edgeCreated, err := ensureEdgeConfig(ctx, deps.Docker, in.Version, render)
		if err != nil {
			return err
		}
		res.EdgeConfig, res.EdgeCreated = edgeName, edgeCreated
		return nil
	})

	wf.Add("Reconciling platform stacks (rendered content vs stored hash)", func(ctx context.Context) error {
		// Render all six platform configs with the fully-populated render
		// (config names + cert secrets substituted) so the snapshots are
		// valid YAML and comparable across runs.
		rendered, err := renderPlatformConfigs(render)
		if err != nil {
			return err
		}

		// Per-stack re-deploy decision: hash the freshly rendered compose and
		// compare it with the stored rendered_hash of the matching config row.
		// Any change in the stack template, the versioned Docker config name it
		// mounts, the edge image tag, or the TLS secret names it references
		// shows up here — the DB hash is the single source of truth.
		//
		// Existence-aware: an unchanged hash alone is NOT sufficient — the
		// stack must also actually exist in the Swarm. A wiped Swarm loses the
		// services even though the store's rendered_hash is unchanged, so a
		// missing stack is redeployed regardless of the hash. A Force run (or a
		// detected swarm-identity change) skips both checks and redeploys
		// everything.
		forceAll := in.Force || swarmChanged
		var redeploy []stackName
		reasons := map[stackName]string{}
		order := []stackName{StackObservability, StackInfra, StackEdge, StackBackup}
		if sso.Enabled {
			order = append(order, StackSSO)
		}
		for _, s := range order {
			cfgName := string(s) + "-stack"
			fresh := rendered[cfgName]
			row, err := deps.Store.GetConfig(ctx, cfgName)
			if err != nil && !errors.Is(err, store.ErrConfigNotFound) {
				return fmt.Errorf("read rendered hash for %s: %w", cfgName, err)
			}
			switch {
			case forceAll:
				reasons[s] = "forced"
			case row == nil || row.RenderedHash != store.ConfigHash(string(fresh)):
				reasons[s] = "content changed"
			default:
				inSync, why, syncErr := platformStackInSync(ctx, deps.Docker, s, fresh)
				if syncErr != nil {
					// A Docker error means we cannot tell — assume in sync so
					// we never redeploy (and thus never infinite-loop) on a
					// flaky API.
					continue
				}
				if inSync {
					continue
				}
				reasons[s] = why
			}
			redeploy = append(redeploy, s)
		}

		// Deploy changed stacks first. Each stack's rendered snapshot
		// (rendered_content + rendered_hash) is stamped into the store only
		// AFTER its deploy succeeds — so a failed deploy leaves the stored
		// hash stale and the NEXT update retries the deploy. (BUG-009: the
		// hash used to be stamped up-front, so a failed deploy permanently
		// skipped redeploy even though the swarm never received the stack.)
		redeployed := map[string]bool{}
		for _, s := range redeploy {
			fmt.Fprintf(out, "  ▶ %s %s → re-deploying\n", string(s), reasons[s])
			if err := deployStack(ctx, out, deps.Deployer, s, render); err != nil {
				return err
			}
			res.StacksDeployed = append(res.StacksDeployed, string(s))
			if s == StackEdge {
				res.EdgeDeployed = true
			}
			cfgName := string(s) + "-stack"
			if content, ok := rendered[cfgName]; ok {
				if err := deps.Store.SetRendered(ctx, cfgName, string(content)); err != nil &&
					!errors.Is(err, store.ErrConfigNotFound) {
					return fmt.Errorf("store rendered config %s: %w", cfgName, err)
				}
			}
			redeployed[cfgName] = true
		}

		// Snapshot the unchanged rendered configs (rendered_content +
		// rendered_hash) so the console reads them back and the next update
		// has a baseline to compare against. Runs after the deploy loop so a
		// failed deploy never leaves a stale hash.
		for name, content := range rendered {
			if redeployed[name] {
				continue
			}
			if err := deps.Store.SetRendered(ctx, name, string(content)); err != nil && !errors.Is(err, store.ErrConfigNotFound) {
				return fmt.Errorf("store rendered config %s: %w", name, err)
			}
		}

		if len(redeploy) == 0 {
			fmt.Fprintf(out, "  ▶ No rendered content changed — nothing to redeploy.\n")
		}
		return nil
	})

	wf.Add("Ensuring storage-node labels match the storage_nodes setting", func(ctx context.Context) error {
		if deps.Docker == nil {
			return nil
		}
		raw := deps.Store.GetSettingDefault(ctx, SettingStorageNodes(), "")
		if raw == "" {
			return nil
		}
		nodes, err := deps.Docker.NodeList(ctx)
		if err != nil {
			return fmt.Errorf("list swarm nodes: %w", err)
		}
		for _, want := range splitStorageNodes(raw) {
			for _, n := range nodes {
				if n.Hostname == want {
					if err := deps.Docker.SetNodeLabel(ctx, n.ID, runtime.StorageNodeLabel, "true"); err != nil {
						fmt.Fprintf(out, "  ⚠ could not label storage node %s (%v)\n", want, err)
					}
					break
				}
			}
		}
		return nil
	})

	wf.Add("Cluster update complete.", func(ctx context.Context) error { return nil })

	if err := wf.Run(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// renderPlatformConfigs renders all six platform configs (four stack composes
// plus otel-collector-config and traefik-dynamic) with a fully populated
// render — the versioned Docker config names, TLS secret names and edge image
// tag are substituted, so every snapshot is valid YAML and stable across runs.
func renderPlatformConfigs(render RenderInput) (map[string][]byte, error) {
	out := make(map[string][]byte, 6)
	stacks := []stackName{StackInfra, StackObservability, StackBackup, StackEdge}
	if render.SSOEnabled {
		stacks = append(stacks, StackSSO)
	}
	for _, s := range stacks {
		y, err := LoadComposeFile(s, render)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", s, err)
		}
		out[string(s)+"-stack"] = y
	}
	nonService, err := renderRenderedConfigs(render)
	if err != nil {
		return nil, err
	}
	for k, v := range nonService {
		out[k] = v
	}
	return out, nil
}

// deployStack renders and deploys a single stack, streaming progress.
func deployStack(ctx context.Context, out io.Writer, d StackDeployer, s stackName, render RenderInput) error {
	composeYAML, err := LoadComposeFile(s, render)
	if err != nil {
		return fmt.Errorf("load compose for %s: %w", s, err)
	}
	fmt.Fprintf(out, "  ▶ docker stack deploy %s\n", s)
	if err := d.DeployStack(ctx, string(s), composeYAML); err != nil {
		return fmt.Errorf("deploy %s: %w", s, err)
	}
	return nil
}

// platformStackInSync reports whether the live Swarm matches the freshly
// rendered platform compose. It is the drift detector the stored rendered hash
// cannot provide: the DB hash records what pmcluster last INTENDED to deploy,
// so a Swarm-side change (a manual `docker service update`, a half-applied
// deploy, an upgrade that skipped one service) leaves the store reporting
// "nothing to redeploy" forever (BUG-017 — the wafaa control-plane backup ran
// for weeks without its offsite/WebDAV destination for exactly this reason).
//
// Two independent signals are checked:
//   - every service the render expects is present in the stack (detects a
//     missing or renamed service), and
//   - a service that carries a RenderedHashLabel whose value differs from the
//     freshly rendered compose's label was deployed from a DIFFERENT render
//     (detects a half-applied deploy, a skipped service, an older binary) —
//     the exact class of drift that let the wafaa control-plane backup run
//     without its offsite destination. A service with NO label predates the
//     label and cannot be compared, so it is treated as in sync; the render
//     change that introduced the label already forced one redeploy that
//     stamped it.
//
// Extra live services the render no longer produces are deliberately ignored —
// `docker stack deploy` cannot remove them, so flagging them would redeploy
// forever. This also cannot see a hand-edited env on a live service (a manual
// service update does not change labels); it detects SWARM-vs-RENDER drift,
// not render-internal tampering.
//
// A nil client is treated as "in sync" so store-only callers never
// force-redeploy. Any error is returned so the caller can decide to
// assume-in-sync (avoiding a redeploy loop).
func platformStackInSync(ctx context.Context, d runtime.Client, s stackName, fresh []byte) (bool, string, error) {
	if d == nil {
		return true, "", nil
	}
	want, wantHash, err := composeServiceNames(fresh)
	if err != nil {
		return false, "", err
	}
	services, err := d.ServiceList(ctx)
	if err != nil {
		return false, "", err
	}
	live := map[string]string{}
	for _, svc := range services {
		if svc.Stack == string(s) {
			live[strings.TrimPrefix(svc.Name, string(s)+"_")] = svc.Labels[runtime.RenderedHashLabel]
		}
	}
	if len(live) == 0 {
		return false, "absent from the swarm", nil
	}
	// Only services the render WANTS are checked. A live service the render no
	// longer produces is ignored on purpose: `docker stack deploy` cannot
	// remove a service absent from the compose (no --prune), so flagging it
	// would re-deploy on every update forever.
	for name := range want {
		got, ok := live[name]
		if !ok {
			return false, "a required service is missing", nil
		}
		if wantHash != "" && got != "" && got != wantHash {
			return false, "drifted from the rendered compose", nil
		}
	}
	return true, "", nil
}

// composeServiceNames parses a rendered compose file and returns its service
// names plus the rendered-hash label stamped on each service ("" when the
// render predates the label, in which case only the service set is compared).
func composeServiceNames(composeYAML []byte) (map[string]struct{}, string, error) {
	var doc struct {
		Services map[string]struct {
			Deploy struct {
				Labels map[string]string `json:"labels"`
			} `json:"deploy"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, "", fmt.Errorf("parse rendered compose: %w", err)
	}
	names := make(map[string]struct{}, len(doc.Services))
	hash := ""
	for n, svc := range doc.Services {
		names[n] = struct{}{}
		if hash == "" {
			hash = svc.Deploy.Labels[runtime.RenderedHashLabel]
		}
	}
	return names, hash, nil
}

// ensureLegacyPlainSecret re-materializes a Swarm secret for a credential that
// predates the current bootstrapSpecs list (e.g. the pre-SeaweedFS
// minio_root_password). The value is recovered from the DB ciphertext and
// mirrored as a plain-format secret; a missing credential row is a no-op. The
// stored swarm_secret_name wins when present, falling back to secretName.
func ensureLegacyPlainSecret(ctx context.Context, deps UpdateDeps, credName, secretName string) (bool, error) {
	if deps.Docker == nil || deps.Store == nil || deps.Cipher == nil {
		return false, nil
	}
	cred, err := deps.Store.GetCredential(ctx, credName)
	if err != nil {
		if errors.Is(err, store.ErrCredentialNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("lookup %s: %w", credName, err)
	}
	plain, err := deps.Cipher.Decrypt(cred.PasswordCiphertext)
	if err != nil {
		return false, fmt.Errorf("decrypt %s: %w", credName, err)
	}
	target := secretName
	if cred.SwarmSecretName != "" {
		target = cred.SwarmSecretName
	}
	return EnsureSecret(ctx, deps.Docker, target, plain)
}

// RenderClusterConfigs renders the current platform configs (post-substitution
// YAML) without deploying any stack. It mirrors Update's render construction —
// including the versioned Docker config names for OTel/Traefik/edge — so the
// output matches what a `cluster update` would deploy and is valid YAML.
// Keep in sync with Update.
func RenderClusterConfigs(ctx context.Context, deps UpdateDeps, in UpdateInput) (map[string]string, error) {
	if err := Preflight(ctx, deps.Docker); err != nil {
		return nil, err
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("rendering configs requires a store (config_dir must be initialised via `pmcluster init`)")
	}
	state, err := loadTLSSettings(ctx, deps.Store)
	if err != nil {
		return nil, err
	}
	domain := deps.Store.GetSettingDefault(ctx, settingDomain, "")
	if domain == "" {
		return nil, fmt.Errorf("no persisted domain found — run `cluster up` before requesting rendered configs")
	}
	ooCred, err := deps.Store.GetCredential(ctx, "openobserve_admin")
	if err != nil {
		return nil, fmt.Errorf("load openobserve_admin credential (run `cluster up` first): %w", err)
	}
	ooPass, err := deps.Cipher.Decrypt(ooCred.PasswordCiphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt openobserve_admin password: %w", err)
	}
	ooL, ooM, ooT := loadOORetention(ctx, deps.Store)
	s3b := loadBackupS3(ctx, deps.Store)
	objStore, objStoreErr := loadObjectStore(ctx, deps.Docker, deps.Store, deps.Cipher)
	if objStoreErr != nil {
		return nil, objStoreErr
	}
	render := RenderInput{
		Domain:                   domain,
		OpenObserveAdminEmail:    ooCred.Username,
		OpenObserveAdminPassword: string(ooPass),
		OpenObserveBasicAuth:     openObserveBasicAuth(ooCred.Username, string(ooPass)),
		OpenObserveSessionCookie: openObserveSessionCookie(ooCred.Username, string(ooPass)),
		ACMEEmail:                state.ACMEEmail,
		ConfigDir:                in.ConfigDir,
		ConfigStore:              deps.Store,
		DataDir:                  filepath.Dir(in.ConfigDir),
		VolumeRoot:               effectiveVolumeRoot(deps.Store.GetSettingDefault(ctx, SettingVolumeRoot(), "")),
		BackupDir:                backupRootDir(),
		EdgeImage:                EdgeImageFor(),
		EdgeLoginDisabled:        loadEdgeLoginDisabled(ctx, deps.Store),
		BackupAllNodes:           loadBackupAllNodes(ctx, deps.Store),
		BackupRetentionDays:      LoadBackupRetentionDays(ctx, deps.Store),
		BackupCron:               LoadBackupCron(ctx, deps.Store),
		StorageNodeConstraint:    deps.Store.GetSettingDefault(ctx, SettingStorageNodes(), "") != "",
		StorageNodeLabel:         runtime.StorageNodeLabel,
		PlatformNode:             loadPlatformNode(ctx, deps.Store),
		OOLogsRetentionDays:      ooL, OOMetricsRetentionDays: ooM, OOTracesRetentionDays: ooT,
		BackupS3: s3b,
		Store:    objStore,
	}
	sso, err := loadSSOSettings(ctx, deps.Store)
	if err != nil {
		return nil, err
	}
	render.SSOEnabled = sso.Enabled
	render.SSOClientID = sso.ClientID
	render.SSOClientSecret = sso.ClientSecret
	render.SSOGitHubOrg = sso.GitHubOrg
	render.SSOGitHubRepos = sso.GitHubRepos
	render.SSOCookieExpire = sso.CookieExpire
	if sso.Enabled {
		cookieCred, err := deps.Store.GetCredential(ctx, "sso_cookie_secret")
		switch {
		case errors.Is(err, store.ErrCredentialNotFound):
			mgr := &CredentialsManager{Store: deps.Store, Cipher: deps.Cipher, Docker: deps.Docker, Deployer: deps.Deployer}
			mc, cerr := mgr.Ensure(ctx, "sso_cookie_secret")
			if cerr != nil {
				return nil, fmt.Errorf("bootstrap sso_cookie_secret (SSO enabled): %w", cerr)
			}
			render.SSOCookieSecret = mc.Password
		case err != nil:
			return nil, fmt.Errorf("load sso_cookie_secret credential (SSO enabled): %w", err)
		default:
			plain, derr := deps.Cipher.Decrypt(cookieCred.PasswordCiphertext)
			if derr != nil {
				return nil, fmt.Errorf("decrypt sso_cookie_secret: %w", derr)
			}
			render.SSOCookieSecret = string(plain)
		}
	}
	hostCerts, err := loadHostCertEntries(ctx, deps.Store, domain)
	if err != nil {
		return nil, err
	}
	render.HostCerts = hostCerts
	if render.ACMEEmail == "" {
		if state.CertPath == "" || state.KeyPath == "" {
			return nil, fmt.Errorf("TLS is not ACME (no ACME email) but no cert/key paths are persisted — run `cluster up` with --cert/--key (or --acme-email)")
		}
		certName, _, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "cert", state.CertPath)
		if err != nil {
			return nil, err
		}
		keyName, _, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "key", state.KeyPath)
		if err != nil {
			return nil, err
		}
		render.CertSecretName, render.KeySecretName = certName, keyName
	}

	// Materialise the versioned Docker config names so the rendered stacks
	// reference real configs (valid YAML) and the snapshots are hashable.
	renderedConfigs, err := EnsureRenderedConfigs(ctx, deps.Docker, &render, in.Version)
	if err != nil {
		return nil, err
	}
	_ = renderedConfigs
	edgeName, _, err := ensureEdgeConfig(ctx, deps.Docker, in.Version, render)
	if err != nil {
		return nil, err
	}
	_ = edgeName

	rendered, err := renderPlatformConfigs(render)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rendered))
	for name, content := range rendered {
		out[name] = string(content)
	}
	return out, nil
}
