package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

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
	// One login at a time. The periodic tick, the leadership-gain mint and a
	// startup replay all write the same file (docker's config.json); two
	// concurrent logins could interleave that write and leave a credential from
	// one host or an empty auths object behind.
	registryLoginMu.Lock()
	loginErr := dockerLoginExec(ctx, ghcrHost, "x-access-token", []byte(tok.Token))
	registryLoginMu.Unlock()
	if loginErr != nil {
		return true, fmt.Errorf("docker login %s with the App token: %w", ghcrHost, loginErr)
	}
	log.Warn().
		Str("registry", ghcrHost).
		Str("user", "x-access-token").
		Time("expires_at", tok.ExpiresAt.UTC()).
		Msgf("docker logged in to %s with a GitHub App token; it expires at %s and is renewed every 25 minutes",
			ghcrHost, tok.ExpiresAt.UTC().Format(time.RFC3339))
	return true, nil
}

// registryLoginMu serialises every `docker login` this package runs against
// docker's config file. A tick and a leadership gain must never overlap.
var registryLoginMu sync.Mutex

// dockerConfigDir is where docker reads its config under the systemd unit. The
// unit runs with a minimal environment, so HOME may be unset or point
// somewhere docker does not look; --with-registry-auth reads the config from
// DOCKER_CONFIG and would otherwise forward nothing.
const dockerConfigDir = "/root/.docker"

// dockerLoginExec is the non-interactive login used for the App token. It is a
// package variable so a test can observe the arguments without a docker
// daemon.
var dockerLoginExec = func(ctx context.Context, host, username string, password []byte) error {
	cmd := exec.CommandContext(ctx, "docker", "login", host, "--username", username, "--password-stdin")
	// The token rides on stdin only: argv is visible in `ps` to every user on
	// the node.
	cmd.Stdin = bytes.NewReader(password)
	cmd.Env = dockerLoginEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Carry docker's own words: "unauthorized", "denied: denied" and
		// "Cannot connect to the Docker daemon" are three different problems,
		// and a generic "login failed" hides which one happened.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("docker login %s: %w", host, err)
		}
		return fmt.Errorf("docker login %s: %w: %s", host, err, msg)
	}
	return nil
}

// dockerLoginEnv pins DOCKER_CONFIG and replaces any inherited value, so the
// login always writes the config a deploy later reads.
func dockerLoginEnv(base []string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, "DOCKER_CONFIG=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "DOCKER_CONFIG="+dockerConfigDir)
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

// loginOnLeadershipGain mints and logs in once on the follower→leader
// transition, before the reconcile passes that re-deploy platform stacks.
//
// Without it, a new leader deploys with whatever credential happened to be on
// disk — possibly one minted by the previous leader an hour ago — and
// `--with-registry-auth` freezes that dead token into every spec it writes.
//
// Not fatal: a failed mint logs loudly and lets the reconcile proceed (a stale
// but present credential still lets the deploy try, and the pull error names
// the real cause), and the 25-minute timer retries.
func loginOnLeadershipGain(ctx context.Context, st *store.Store, cfg *config.Config, log zerolog.Logger) {
	if _, err := githubAppLogin(ctx, st, cfg, log); err != nil {
		log.Warn().Err(err).Msg("docker login to ghcr.io on leadership gain failed; deploying with whatever credential docker already holds (the 25-minute timer retries)")
	}
}

// refreshGitHubAppLogins keeps the GHCR login fresh. A token lives an hour, so
// a manager that only logged in at startup would fail private pulls an hour
// later — long before the next deploy notices.
//
// It is a no-op when no app is configured. Failures are logged and retried on
// the next tick: a GitHub outage must not stop deploys that use public images
// or a stored credential.
func refreshGitHubAppLogins(ctx context.Context, st *store.Store, cfg *config.Config, log zerolog.Logger, every time.Duration) {
	if every <= 0 {
		// 25 minutes, not 40: a token minted at t=0 expires at t=60, and the
		// mint refreshes at t=50. A 40-minute tick would log the SAME token in
		// at t=40 (still cached) and only mint a new one at t=80 — twenty
		// minutes after the docker config's token had already expired. Two
		// ticks per token lifetime keeps a fresh credential in docker's config
		// with at least 10 minutes of headroom at every tick.
		every = 25 * time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := githubAppLogin(ctx, st, cfg, log); err != nil {
				log.Warn().Err(err).Msg("GitHub App registry re-login had issues; private images may fail to pull")
			}
		}
	}
}
