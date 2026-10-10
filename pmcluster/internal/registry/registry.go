// Package registry stores Docker registry credentials for private image
// pulls: the credential is encrypted in SQLite AND mirrored into
// ~/.docker/config.json via `docker login`, so `docker stack deploy
// --with-registry-auth` forwards the auth to every node that needs to pull.
//
// Both the CLI (`pmcluster registry …`) and the console API go through this
// one implementation, so the two can never drift apart.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// ErrInvalidInput marks request-validation failures (a missing field, no
// encryption key, no docker runner) so the HTTP layer can answer 400 rather
// than blaming the upstream registry.
var ErrInvalidInput = errors.New("invalid input")

// Entry is the metadata view of a configured registry. Password material
// never appears here — it stays encrypted in the store and (mirrored) in the
// docker CLI's own config.
type Entry struct {
	Host      string `json:"host"`
	Username  string `json:"username"`
	CreatedAt int64  `json:"created_at"`
}

// Store is the persistence port for registry rows (satisfied by *store.Store).
type Store interface {
	CreateRegistry(ctx context.Context, r *store.Registry) error
	UpdateRegistry(ctx context.Context, r *store.Registry) error
	GetRegistry(ctx context.Context, host string) (*store.Registry, error)
	ListRegistries(ctx context.Context) ([]*store.Registry, error)
	DeleteRegistry(ctx context.Context, host string) error
}

// Runner performs the host-side login/logout that makes stored credentials
// usable by the Swarm.
type Runner interface {
	Login(ctx context.Context, host, username, password string) error
	Logout(ctx context.Context, host string) error
}

// Service is the registry-credential use case.
type Service struct {
	Store  Store
	Cipher *credentials.Cipher
	Runner Runner
}

// List returns every configured registry, without any credential material.
func (s *Service) List(ctx context.Context) ([]Entry, error) {
	rows, err := s.Store.ListRegistries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list registries: %w", err)
	}
	out := make([]Entry, 0, len(rows))
	for _, r := range rows {
		out = append(out, Entry{Host: r.Host, Username: r.Username, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// Add logs in to the registry on this host, then records the credential.
// The login runs FIRST so a wrong password never leaves a row behind that
// claims to work. Returns whether the host was newly configured.
func (s *Service) Add(ctx context.Context, host, username, password string) (bool, error) {
	host = strings.TrimSpace(host)
	username = strings.TrimSpace(username)
	switch {
	case host == "":
		return false, fmt.Errorf("%w: host is required", ErrInvalidInput)
	case username == "":
		return false, fmt.Errorf("%w: username is required", ErrInvalidInput)
	case password == "":
		return false, fmt.Errorf("%w: password is required", ErrInvalidInput)
	}
	if s.Cipher == nil {
		return false, fmt.Errorf("%w: encryption key unavailable; cannot store the credential", ErrInvalidInput)
	}
	if s.Runner == nil {
		return false, fmt.Errorf("%w: docker login is not configured on this node", ErrInvalidInput)
	}
	if err := s.Runner.Login(ctx, host, username, password); err != nil {
		return false, fmt.Errorf("docker login %s: %w", host, err)
	}
	ciphertext, err := s.Cipher.Encrypt([]byte(password))
	if err != nil {
		return false, fmt.Errorf("encrypt password: %w", err)
	}
	row := &store.Registry{Host: host, Username: username, PasswordCiphertext: ciphertext}
	existing, err := s.Store.GetRegistry(ctx, host)
	if err != nil && !errors.Is(err, store.ErrRegistryNotFound) {
		return false, err
	}
	if existing != nil {
		return false, s.Store.UpdateRegistry(ctx, row)
	}
	return true, s.Store.CreateRegistry(ctx, row)
}

// Remove drops the stored credential and logs the host out. A failing logout
// is reported back as a warning rather than failing the removal: the row is
// already gone, and the operator needs to know the docker config still holds
// stale auth.
func (s *Service) Remove(ctx context.Context, host string) (warning string, err error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("%w: host is required", ErrInvalidInput)
	}
	if err := s.Store.DeleteRegistry(ctx, host); err != nil {
		return "", err
	}
	if s.Runner != nil {
		if logoutErr := s.Runner.Logout(ctx, host); logoutErr != nil {
			return fmt.Sprintf("docker logout %s: %v", host, logoutErr), nil
		}
	}
	return "", nil
}

// ExecRunner shells out to the docker CLI. Out/Err receive the command's own
// output (nil discards it). The password is passed on stdin only, never as an
// argument, so it cannot leak through the process list.
type ExecRunner struct {
	Out io.Writer
	Err io.Writer
}

// Login runs `docker login <host> -u <user> --password-stdin`.
func (r ExecRunner) Login(ctx context.Context, host, username, password string) error {
	cmd := exec.CommandContext(ctx, "docker", "login", host, "-u", username, "--password-stdin")
	cmd.Stdin = strings.NewReader(password)
	cmd.Stdout = orDiscard(r.Out)
	cmd.Stderr = orDiscard(r.Err)
	return cmd.Run()
}

// Logout runs `docker logout <host>`, folding the command's output into the
// error so the caller can show why it failed.
func (r ExecRunner) Logout(ctx context.Context, host string) error {
	out, err := exec.CommandContext(ctx, "docker", "logout", host).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
