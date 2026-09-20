package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// replayRegistryLogins re-runs docker login for every persisted registry
// so a manager rebuild (SQLite survived, ~/.docker/config.json gone)
// keeps pulling private images. Best-effort: failures are logged, never
// block the daemon.
func replayRegistryLogins(ctx context.Context, st *store.Store, cfg *config.Config, log zerolog.Logger) error {
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

	var problems []string
	for _, r := range regs {
		password, err := cipher.Decrypt(r.PasswordCiphertext)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: decrypt: %v", r.Host, err))
			continue
		}
		cmd := exec.CommandContext(ctx, "docker", "login", r.Host, "--username", r.Username, "--password-stdin")
		cmd.Stdin = bytes.NewReader(password)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v (%s)", r.Host, err, strings.TrimSpace(stderr.String())))
			continue
		}
		log.Info().Str("registry", r.Host).Str("user", r.Username).Msg("registry re-login OK")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}
