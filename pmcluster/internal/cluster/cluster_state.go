package cluster

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// cluster_settings keys persisted on a successful `up` and read back by both
// `up` (idempotent re-runs) and `update` (knows what to re-provision without
// re-bootstrapping).
const (
	settingTLSMode     = "tls_mode"
	settingTLSCertPath = "tls_cert_path"
	settingTLSKeyPath  = "tls_key_path"
	settingTLSACME     = "tls_acme_email"
	settingDomain      = "domain"
	settingOOEmail     = "oo_admin_email"
)

// clusterInstalled reports whether this store already holds a live cluster.
// `cluster up` is init-only, so it refuses to run when either the persisted
// domain/TLS install state or the cluster-scope platform config rows exist.
func clusterInstalled(ctx context.Context, st *store.Store) (bool, error) {
	if st == nil {
		return false, nil
	}
	if st.GetSettingDefault(ctx, settingDomain, "") != "" {
		return true, nil
	}
	if st.GetSettingDefault(ctx, settingTLSMode, "") != "" {
		return true, nil
	}
	rows, err := st.ListConfigs(ctx, "cluster", "")
	if err != nil {
		return false, fmt.Errorf("list cluster configs: %w", err)
	}
	return len(rows) > 0, nil
}

// tlsState is the persisted TLS install state.
type tlsState struct {
	Mode      string
	CertPath  string
	KeyPath   string
	ACMEEmail string
}

// loadTLSSettings reads the persisted TLS state (empty when never installed).
func loadTLSSettings(ctx context.Context, st *store.Store) (tlsState, error) {
	if st == nil {
		return tlsState{}, nil
	}
	return tlsState{
		Mode:      st.GetSettingDefault(ctx, settingTLSMode, ""),
		CertPath:  st.GetSettingDefault(ctx, settingTLSCertPath, ""),
		KeyPath:   st.GetSettingDefault(ctx, settingTLSKeyPath, ""),
		ACMEEmail: st.GetSettingDefault(ctx, settingTLSACME, ""),
	}, nil
}

func (t tlsState) save(ctx context.Context, st *store.Store) error {
	if st == nil {
		return nil
	}
	vals := map[string]string{
		settingTLSMode:     t.Mode,
		settingTLSCertPath: t.CertPath,
		settingTLSKeyPath:  t.KeyPath,
		settingTLSACME:     t.ACMEEmail,
	}
	for k, v := range vals {
		if err := st.SetSetting(ctx, k, v); err != nil {
			return fmt.Errorf("persist %s: %w", k, err)
		}
	}
	return nil
}

// requestedTLSMode derives the TLS mode the operator asked for on this run,
// or "" if no TLS flags were supplied (meaning "use the stored mode").
func requestedTLSMode(in UpInput) string {
	if in.ACMEEmail != "" {
		return "acme"
	}
	if in.CertPath != "" || in.KeyPath != "" {
		return "cert"
	}
	return ""
}

// mergeTLSState reconciles the operator's requested input with the stored
// install state so an idempotent re-run doesn't require re-passing the TLS
// flags and can't silently flip the TLS mode.
//
//   - No stored state → input is used as-is.
//   - Stored state + no TLS flags on this run → fill in the stored cert/key
//     paths (cert mode) or ACME email (acme mode).
//   - Stored mode != requested mode → error unless ForceTLSMode is set.
//   - Stored mode == requested mode → any flags supplied win, others are
//     filled from storage.
func mergeTLSState(in UpInput, state tlsState) (UpInput, error) {
	if state.Mode == "" {
		return in, nil
	}
	req := requestedTLSMode(in)
	if req != "" && req != state.Mode && !in.ForceTLSMode {
		return in, fmt.Errorf("cluster is installed with TLS mode %q but you requested %q; pass --force-tls-mode to switch (an accidental mode flip can permanently break the edge)", state.Mode, req)
	}
	switch state.Mode {
	case "cert":
		if in.CertPath == "" {
			in.CertPath = state.CertPath
		}
		if in.KeyPath == "" {
			in.KeyPath = state.KeyPath
		}
	case "acme":
		if in.ACMEEmail == "" {
			in.ACMEEmail = state.ACMEEmail
		}
	}
	return in, nil
}
