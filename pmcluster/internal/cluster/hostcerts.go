package cluster

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// HostCertsDeps bundles the collaborators a per-host cert refresh needs.
// It mirrors UpdateDeps minus stdout (refresh is silent).
type HostCertsDeps struct {
	Store       *store.Store
	Cipher      *credentials.Cipher
	Docker      docker.Client
	Deployer    StackDeployer
	Provisioner *OpenObserveProvisioner
}

// HostCertEntry is one per-host cert's versioned Swarm secret names, used by
// the Traefik dynamic-config render (tls.certificates reference
// /run/secrets/<name> — never on-disk files). The cluster's own domain is NOT
// listed here: it is rendered by the template's default-cert block.
type HostCertEntry struct {
	Host       string
	CertSecret string
	KeySecret  string
}

// hostSecretBase derives the versioned-secret base name for a per-host
// cert/key. The host is lowercased and every non-alphanumeric character is
// collapsed to "-" so the resulting name satisfies the Docker secret charset
// ([a-zA-Z0-9][a-zA-Z0-9_.-]*) even for wildcard hosts like "*.example.com".
// Example: idlebbookfair.com → "hostcert-idlebbookfair-com" / "hostkey-idlebbookfair-com".
//
// The base name MUST differ per host: cert_vN/key_vN hold exactly one cert
// each, so sharing a base between two hosts would mint a new version and GC
// the first host's cert. The mechanism is identical to the main cert
// (versioned, content-aware, hash-labeled); only the name is scoped per host.
func hostSecretBase(kind, host string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return "host" + kind + "-" + b.String()
}

// loadHostCertEntries reads every stored certificate row EXCEPT the cluster's
// own domain (that one is the main cert, rendered by the template) and maps
// it to the secret names the Traefik dynamic-config render appends. Nil store
// yields nil (no per-host certs).
func loadHostCertEntries(ctx context.Context, st *store.Store, domain string) ([]HostCertEntry, error) {
	if st == nil {
		return nil, nil
	}
	rows, err := st.ListSiteCerts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list certificates: %w", err)
	}
	out := make([]HostCertEntry, 0, len(rows))
	for _, r := range rows {
		if r.Domain == domain {
			continue
		}
		out = append(out, HostCertEntry{Host: r.Domain, CertSecret: r.CertSecret, KeySecret: r.KeySecret})
	}
	return out, nil
}

// RefreshHostCerts re-renders the Traefik dynamic config — which includes the
// per-host certs recorded in the DB — and, if the content moved, pushes a new
// versioned Docker config and re-deploys the infra stack so Traefik serves
// the per-host certs. It is called by the CLI and the daemon API after a
// per-host cert is added or removed.
//
// It reuses the full Update pipeline, which already computes the cluster's own
// TLS secret names and re-deploys only when the dynamic content actually
// changed (a no-op refresh deploys nothing). Requires the cluster to be up
// (persisted domain + OO credential).
func RefreshHostCerts(ctx context.Context, deps HostCertsDeps, configDir, version string) (traefikCreated bool, err error) {
	if deps.Store == nil {
		return false, fmt.Errorf("refresh host certs requires a store (run `pmcluster init` + `pmcluster cluster up` first)")
	}
	res, err := Update(ctx, UpdateDeps{
		Store:       deps.Store,
		Cipher:      deps.Cipher,
		Docker:      deps.Docker,
		Deployer:    deps.Deployer,
		Provisioner: deps.Provisioner,
		Stdout:      io.Discard,
	}, UpdateInput{ConfigDir: configDir, Version: version})
	if err != nil {
		return false, err
	}

	return res.TraefikCreated || res.CertCreated || res.KeyCreated, nil
}
