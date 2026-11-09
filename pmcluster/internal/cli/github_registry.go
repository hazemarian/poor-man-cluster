package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// ghcrHost is the registry the App token authenticates against. `docker stack
// deploy --with-registry-auth` forwards this login to every node, exactly like
// a `pmcluster registry add ghcr.io` credential.
const ghcrHost = "ghcr.io"

// newGitHubClient is the daemon/CLI API client. A package variable so tests can
// point it at an httptest server; production never writes to it.
var newGitHubClient = func(log *zerolog.Logger) *githubapp.Client {
	return &githubapp.Client{Log: log}
}

// githubAppLogin mints an installation token and runs
// `docker login ghcr.io -u x-access-token --password-stdin`.
//
// The token is passed on stdin and lives in memory only. Docker's own config
// then holds it for `--with-registry-auth`, which is the mechanism the existing
// registry path already relies on; pmcluster itself never writes the token to
// data.db or renders it into a compose file.
//
// Returns configured=false when no app is registered: that is not an error and
// the caller must fall back to the stored registry credential.
func githubAppLogin(ctx context.Context, st *store.Store, cfg *config.Config, log zerolog.Logger) (configured bool, err error) {
	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return false, fmt.Errorf("open encryption key: %w", err)
	}
	l := newGitHubAppLocal(st, cipher, log)
	if !l.Configured(ctx) {
		return false, nil
	}
	tok, err := l.Token(ctx)
	if err != nil {
		if errors.Is(err, githubapp.ErrNoApp) {
			return false, nil
		}
		return true, err
	}
	if err := dockerLoginExec(ctx, ghcrHost, "x-access-token", []byte(tok.Token)); err != nil {
		return true, fmt.Errorf("docker login %s with the App token: %w", ghcrHost, err)
	}
	log.Info().Str("registry", ghcrHost).Str("user", "x-access-token").
		Time("expires_at", tok.ExpiresAt.UTC()).
		Msg("registry login via GitHub App")
	return true, nil
}

// dockerLoginExec is the non-interactive login used for the App token. It is a
// package variable so a test can observe the arguments without a docker
// daemon.
var dockerLoginExec = func(ctx context.Context, host, username string, password []byte) error {
	cmd := exec.CommandContext(ctx, "docker", "login", host, "--username", username, "--password-stdin")
	cmd.Stdin = bytes.NewReader(password)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (%s)", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// registryLoginTargets drops the stored ghcr.io credential when the App token
// was applied: the app is the intended credential for that host, and logging in
// twice would leave whichever ran last in docker's config. Every other host is
// untouched — the existing path keeps working exactly as before.
//
// This is the fallback rule in one place: no app (or a failed app login) means
// the stored credential is kept.
func registryLoginTargets(regs []*store.Registry, appApplied bool) []*store.Registry {
	if !appApplied {
		return regs
	}
	out := make([]*store.Registry, 0, len(regs))
	for _, r := range regs {
		if r.Host == ghcrHost {
			continue
		}
		out = append(out, r)
	}
	return out
}
