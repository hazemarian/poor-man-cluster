package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Legacy OpenObserve names. These are the ONLY places the old spelling exists
// after the Parseable swap — everything else (settings, credential spec, docs)
// uses the observability names. Kept here so the migration and the constants it
// migrates to cannot drift apart.
const (
	legacyObservabilityCredential = "openobserve_admin"
	legacyObservabilitySecretBase = "zo_root_user_password"
	legacyObservabilityKind       = "openobserve"
)

// legacySettingRenames maps each pre-Parseable setting key to its replacement.
var legacySettingRenames = map[string]string{
	"oo_admin_email":            "observability_admin_email",
	"oo_logs_retention_days":    "observability_logs_retention_days",
	"oo_metrics_retention_days": "observability_metrics_retention_days",
	"oo_traces_retention_days":  "observability_traces_retention_days",
}

// NamingMigrationReport records what a migration run actually changed, so the
// operator sees it in the update output instead of having to trust silence.
type NamingMigrationReport struct {
	CredentialRenamed bool
	SettingsRenamed   []string
	SecretsRemoved    []string
}

// MigrateObservabilityNaming rewrites the OpenObserve-era names that predate the
// Parseable swap: the managed credential row, four cluster settings, and the
// stale Swarm secrets.
//
// It is idempotent and deliberately conservative — it only ever acts when the
// NEW name is absent and the OLD one present, so a partially-migrated cluster
// converges and a fully-migrated one is a no-op. The credential is re-inserted
// under its new name BEFORE the old row is deleted, and the ciphertext is
// carried over untouched: this must never rotate the password, because the
// running Parseable, the collector's exporter and the sign-in bridge all
// authenticate with it, and the operator's browser session depends on it.
func MigrateObservabilityNaming(ctx context.Context, st *store.Store, dc runtime.Client) (NamingMigrationReport, error) {
	var rep NamingMigrationReport

	// ── credential row ──
	if st != nil {
		if _, err := st.Credential(ctx, observabilityCredentialName); err != nil {
			if !errors.Is(err, store.ErrCredentialNotFound) {
				return rep, fmt.Errorf("read %s credential: %w", observabilityCredentialName, err)
			}
			old, err := st.Credential(ctx, legacyObservabilityCredential)
			if err == nil {
				// Preserve username, ciphertext and creation time; adopt the new
				// kind and Secret name only.
				if err := st.InsertCredential(ctx, &store.ManagedCredential{
					Name:               observabilityCredentialName,
					Kind:               string(KindObservability),
					Username:           old.Username,
					PasswordCiphertext: old.PasswordCiphertext,
					SwarmSecretName:    observabilitySecretBase,
				}); err != nil {
					return rep, fmt.Errorf("insert %s credential: %w", observabilityCredentialName, err)
				}
				if err := st.DeleteCredential(ctx, legacyObservabilityCredential); err != nil &&
					!errors.Is(err, store.ErrCredentialNotFound) {
					return rep, fmt.Errorf("delete legacy credential: %w", err)
				}
				rep.CredentialRenamed = true
			} else if !errors.Is(err, store.ErrCredentialNotFound) {
				return rep, fmt.Errorf("read legacy credential: %w", err)
			}
		}

		// ── settings ──
		for oldKey, newKey := range legacySettingRenames {
			if _, err := st.Setting(ctx, newKey); err == nil {
				continue // already migrated
			} else if !errors.Is(err, store.ErrSettingNotFound) {
				return rep, fmt.Errorf("read setting %s: %w", newKey, err)
			}
			v, err := st.Setting(ctx, oldKey)
			if errors.Is(err, store.ErrSettingNotFound) {
				continue // nothing to carry over
			}
			if err != nil {
				return rep, fmt.Errorf("read setting %s: %w", oldKey, err)
			}
			if err := st.SetSetting(ctx, newKey, v); err != nil {
				return rep, fmt.Errorf("write setting %s: %w", newKey, err)
			}
			if err := st.DeleteSetting(ctx, oldKey); err != nil {
				return rep, fmt.Errorf("delete setting %s: %w", oldKey, err)
			}
			rep.SettingsRenamed = append(rep.SettingsRenamed, fmt.Sprintf("%s -> %s", oldKey, newKey))
		}
	}

	// ── stale Swarm secrets ──
	// The unversioned base plus every rotated variant (name_<sha8>). Removing the
	// base first is not enough: rotation leaves versions behind, which is how
	// three zo_root_user_password* secrets ended up on the live node.
	if dc != nil {
		names, err := dc.SecretList(ctx, "", "")
		if err == nil {
			for _, n := range names {
				if n == legacyObservabilitySecretBase ||
					strings.HasPrefix(n, legacyObservabilitySecretBase+"_") {
					if err := dc.SecretRemove(ctx, n); err == nil {
						rep.SecretsRemoved = append(rep.SecretsRemoved, n)
					}
				}
			}
		}
	}

	return rep, nil
}
