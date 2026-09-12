package cluster

import (
	"context"
	"fmt"
	"io"

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

// RefreshHostCerts re-renders the Traefik dynamic config — which includes the
// on-disk per-host certs under <configDir>/hosts — and, if the content moved,
// pushes a new versioned Docker config and re-deploys the infra stack so
// Traefik serves the new per-host certs. It is called by the CLI and the
// daemon API after a per-host cert is added or removed.
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
	// Traefik must reload whenever its dynamic config (host certs) changed or
	// when the cluster's own cert/key changed.
	return res.TraefikCreated || res.CertCreated || res.KeyCreated, nil
}
