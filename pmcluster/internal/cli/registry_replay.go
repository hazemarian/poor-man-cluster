package cli

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// replayRegistryLogins re-runs docker login for every persisted registry so a
// manager rebuild (SQLite survived, ~/.docker/config.json gone) keeps pulling
// private images. Best-effort: failures are logged, never block the daemon.
//
// This is the startup half of the same mechanism the deploy hook uses
// (loginBeforeDeploy): the stored credential is the pull credential, and it is
// a long-lived read-only PAT, so applying it once at boot and once per deploy
// is enough. Nothing here mints a GitHub App token — the App covers
// clone/build/broker, not image pulls.
func replayRegistryLogins(ctx context.Context, st *store.Store, cfg *config.Config, log zerolog.Logger) error {
	return applyStoredRegistryLogins(ctx, st, cfg, log, false)
}
