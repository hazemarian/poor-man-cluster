package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// ghcrHost is the registry the pull credential authenticates against. `docker
// stack deploy --with-registry-auth` forwards this login to every node, exactly
// like any other `pmcluster registry add <host>` credential.
const ghcrHost = "ghcr.io"

// dockerConfigDir is where docker reads its config under the systemd unit. The
// unit runs with a minimal environment, so HOME may be unset or point
// somewhere docker does not look; --with-registry-auth reads the config from
// DOCKER_CONFIG and would otherwise forward nothing.
const dockerConfigDir = "/root/.docker"

// registryNow is the clock the age warnings read. A package variable so a test
// can age a credential without waiting two months.
var registryNow = time.Now

// registryLoginMu serialises every `docker login` this package runs against
// docker's config file. A deploy and a listener's request both write the same
// file; two concurrent logins could interleave that write and leave an empty
// auths object behind.
var registryLoginMu sync.Mutex

// dockerLoginExec is the non-interactive login. It is a package variable so a
// test can observe the arguments without a docker daemon.
var dockerLoginExec = func(ctx context.Context, host, username string, password []byte) error {
	cmd := exec.CommandContext(ctx, "docker", "login", host, "--username", username, "--password-stdin")
	// The password rides on stdin only: argv is visible in `ps` to every user
	// on the node.
	cmd.Stdin = bytes.NewReader(password)
	cmd.Env = dockerLoginEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Carry docker's own words: "unauthorized" and "Cannot connect to the
		// Docker daemon" are different problems, and a generic "login failed"
		// hides which one happened.
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

// loginStoredRegistry applies one stored credential to docker's config under
// the shared mutex.
func loginStoredRegistry(ctx context.Context, r *store.Registry, password []byte) error {
	registryLoginMu.Lock()
	defer registryLoginMu.Unlock()
	return dockerLoginExec(ctx, r.Host, r.Username, password)
}

// applyStoredRegistryLogins runs `docker login` for every credential in the
// control-plane store. It is the single mechanism behind both a manager
// rebuild's startup replay and the deploy-time login; there is no App-token
// login on this path, because the App covers clone/build/broker and a pull
// credential must outlive the service spec that freezes it.
//
// forDeploy=true announces each successful login at WARN with the host and the
// credential's age — that line is the operator's early warning that a
// read-only PAT is approaching its 90-day expiry. forDeploy=false is the
// startup replay and logs at info.
//
// Failures are collected, never fatal: a deploy with a stale credential must
// still get docker's own pull error, which names the registry, rather than a
// pre-flight refusal that hides it. The doctor check is what reports a missing
// or ageing credential before a deploy trips over it.
func applyStoredRegistryLogins(ctx context.Context, st *store.Store, cfg *config.Config, log zerolog.Logger, forDeploy bool) error {
	regs, err := st.ListRegistries(ctx)
	if err != nil {
		return fmt.Errorf("list registries: %w", err)
	}
	if len(regs) == 0 {
		return nil
	}
	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return fmt.Errorf("open encryption key: %w", err)
	}

	now := registryNow()
	var problems []string
	for _, r := range regs {
		password, err := cipher.Decrypt(r.PasswordCiphertext)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: decrypt: %v", r.Host, err))
			continue
		}
		if err := loginStoredRegistry(ctx, r, password); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", r.Host, err))
			if forDeploy {
				log.Warn().Err(err).Str("registry", r.Host).Str("credential_source", "stored registry credential").
					Msgf("docker login to %s with the stored registry credential failed; private pulls will fail with docker's own error (rotate with `pmcluster registry add %s`)", r.Host, r.Host)
			}
			continue
		}
		if forDeploy {
			age := credentialAge(now, r.CreatedAt)
			log.Warn().
				Str("registry", r.Host).
				Str("credential_source", "stored registry credential").
				Str("credential_age", age).
				Msgf("docker logged in to %s using the stored registry credential (added %s); read-only PATs expire at 90 days — rotate with `pmcluster registry add %s`",
					r.Host, age, r.Host)
			continue
		}
		log.Info().Str("registry", r.Host).Str("user", r.Username).Msg("registry re-login OK")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// loginBeforeDeploy is the deploy hook: exactly one login pass per stack
// deploy, before the compose is handed to `docker stack deploy
// --with-registry-auth`. The credential is a long-lived read-only PAT, so the
// copy frozen into a service spec stays valid; that is why nothing refreshes it
// on a timer.
//
// Best-effort by design: a warning, never a blocked deploy.
func loginBeforeDeploy(ctx context.Context, st *store.Store, cfg *config.Config, log zerolog.Logger) {
	if err := applyStoredRegistryLogins(ctx, st, cfg, log, true); err != nil {
		log.Warn().Err(err).Msg("stored registry credentials were not fully applied before this deploy; private pulls may fail")
	}
}

// credentialAge renders how long ago a stored credential was registered, the
// age the deploy warning and the doctor check report. A missing or future
// timestamp is described honestly rather than rendered as a negative age; the
// registry row always records created_at (migrations/0004_webhooks_registries.sql),
// and a rotation resets it, so this is the age of the CURRENT credential.
func credentialAge(now time.Time, createdAt int64) string {
	if createdAt <= 0 {
		return "at an unknown time"
	}
	d := now.Sub(time.Unix(createdAt, 0))
	if d < 0 {
		d = 0
	}
	if d < 24*time.Hour {
		return "less than a day ago"
	}
	days := int(d / (24 * time.Hour))
	if days == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", days)
}
