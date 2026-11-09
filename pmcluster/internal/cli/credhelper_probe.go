package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/pullcred"
)

// credHelperProbe runs the node's docker-credential-pmcluster the way docker
// does, which is the only way to check the pull path end to end: the helper
// reads the swarm node certificate, the edge verifies it, the daemon signs the
// App JWT, GitHub issues the token. Every other check in `doctor` stops at one
// of those hops.
type credHelperProbe struct {
	// Path overrides the helper's location (tests). Empty uses
	// cluster.CredHelperBin, then $PATH.
	Path string
	// Timeout bounds the run. Zero uses 10s — the helper itself dials with a 2s
	// budget, so this only covers a wedged binary.
	Timeout time.Duration
}

// GHCRToken asks the helper for a GHCR credential and returns the secret. The
// secret is for the caller to confirm non-empty; it is never logged or printed.
func (p credHelperProbe) GHCRToken(ctx context.Context) (string, error) {
	path, err := p.resolve()
	if err != nil {
		return "", err
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "get")
	cmd.Stdin = strings.NewReader(pullcred.Registry + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("run %s get %s: %s", path, pullcred.Registry, msg)
	}

	var cred struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &cred); err != nil {
		// The helper's stdout is a contract; something else on it is the bug.
		return "", fmt.Errorf("the helper printed something that is not a credential: %w", err)
	}
	if cred.Secret == "" {
		return "", errors.New("the helper returned an empty token")
	}
	return cred.Secret, nil
}

// resolve locates the helper: the configured path, then $PATH. A missing helper
// is named with the command that installs it, because "executable file not
// found" leaves an operator guessing.
func (p credHelperProbe) resolve() (string, error) {
	if p.Path != "" {
		return p.Path, nil
	}
	if _, err := os.Stat(cluster.CredHelperBin); err == nil {
		return cluster.CredHelperBin, nil
	}
	if found, err := exec.LookPath("docker-credential-" + cluster.CredHelperSuffix); err == nil {
		return found, nil
	}
	return "", fmt.Errorf("%s is not installed on this node (run `pmcluster cluster update` on it, or `pmcluster up` for a fresh install)", cluster.CredHelperBin)
}
