package cluster

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"golang.org/x/crypto/bcrypt"
)

// dataHash returns the stable hex sha256 of a config/secret's payload, used
// as a content fingerprint (pmcluster.data_hash label) for at-a-glance diffing.
func dataHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// EnsureSecret creates a Swarm secret if one with this name doesn't
// exist; returns true iff newly created. Existing secrets are NEVER
// modified — rotation is "remove + recreate + force-redeploy", handled
// by CredentialsManager.Rotate.
//
// pmcluster-managed secrets carry the pmclusterLabel so `cluster down
// --purge` can target them without touching operator-created secrets.
func EnsureSecret(ctx context.Context, d docker.Client, name string, data []byte) (created bool, err error) {
	exists, err := d.SecretExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("check secret %s: %w", name, err)
	}
	if exists {
		return false, nil
	}
	err = d.SecretCreate(ctx, docker.SecretSpec{
		Name: name,
		Data: data,
		Labels: map[string]string{
			pmclusterLabel: "true",
		},
	})
	if err != nil {
		return false, fmt.Errorf("create secret %s: %w", name, err)
	}
	return true, nil
}

func EnsureSecretFromFile(ctx context.Context, d docker.Client, name, path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return EnsureSecret(ctx, d, name, data)
}

// EnsureVersionedSecretFromFile reads a file and provisions a versioned
// Swarm secret (e.g. cert_v001, cert_v002). Returns the versioned name and
// whether a new version was created (false when the file's bytes are unchanged
// and the current version is reused).
func EnsureVersionedSecretFromFile(ctx context.Context, d docker.Client, baseName, path string) (versionedName string, created bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return EnsureVersionedSecret(ctx, d, baseName, data)
}

// EnsureVersionedSecret provisions a versioned Swarm secret (e.g. cert_v001)
// and garbage-collects old versions. It is content-aware: if the current
// highest version already holds exactly these bytes, that version is reused
// (created=false, no GC) so unchanged certs don't mint fresh versions. Same
// pattern as EnsureConfig.
func EnsureVersionedSecret(ctx context.Context, d docker.Client, baseName string, data []byte) (versionedName string, created bool, err error) {
	existing, err := d.SecretList(ctx, pmclusterLabel, "true")
	if err != nil {
		return "", false, fmt.Errorf("list secrets: %w", err)
	}

	prefix := baseName + "_v"
	maxVer := 0
	for _, name := range existing {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		var v int
		if _, scanErr := fmt.Sscanf(name, prefix+"%d", &v); scanErr == nil && v > maxVer {
			maxVer = v
		}
	}

	// Reuse the current version when its content matches — no churn.
	//
	// NOTE: Docker never returns secret payloads on inspect (secrets are
	// write-only by design), so payload comparisons against a real daemon
	// always fail. We compare the pmcluster.data_hash label fingerprint
	// instead, which is recorded at creation time.
	if maxVer > 0 {
		curName := fmt.Sprintf("%s_v%03d", baseName, maxVer)
		if cur, err := d.SecretInspect(ctx, curName); err == nil {
			if h, ok := cur.Labels["pmcluster.data_hash"]; ok && h == dataHash(data) {
				return curName, false, nil
			}
		}
	}

	newVer := maxVer + 1
	versionedName = fmt.Sprintf("%s_v%03d", baseName, newVer)

	err = d.SecretCreate(ctx, docker.SecretSpec{
		Name: versionedName,
		Data: data,
		Labels: map[string]string{
			pmclusterLabel:        "true",
			"pmcluster.base":      baseName,
			"pmcluster.data_hash": dataHash(data),
		},
	})
	if err != nil {
		return "", false, fmt.Errorf("create secret %s: %w", versionedName, err)
	}

	// GC old versions.
	for _, name := range existing {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if name == versionedName {
			continue
		}
		if rmErr := d.SecretRemove(ctx, name); rmErr != nil {
			_ = rmErr // old secret may still be in use; GC next time
		}
	}

	return versionedName, true, nil
}

// EnsureConfig is the Docker-config analogue of EnsureSecret. Because Docker
// configs are immutable, we create a versioned name (e.g.
// pmcluster_otel_config_v001) and let the caller embed the versioned name
// into the compose file via __CONFIG_NAME__ placeholder. Old versions are
// cleaned up after the new one is created. Returns the full versioned name
// actually used and whether a NEW version was created (false when the render
// is unchanged and the current version is reused).
//
// The provisioning is content-aware: if the current highest version already
// holds exactly these bytes, that version is reused (created=false, no GC) so
// an unchanged render doesn't mint fresh versions or churn consumers. Only a
// byte change bumps to baseName_vN+1 and GCs the older ones.
//
// baseName is the logical name ("pmcluster_otel_config"). The versioned name
// is baseName + "_v" + zero-padded sequence.
func EnsureConfig(ctx context.Context, d docker.Client, baseName string, data []byte, version string) (versionedName string, created bool, err error) {
	// List ALL configs, then own the ones matching our versioned name prefix
	// by name rather than by label. We deliberately don't filter by the
	// pmcluster label here: a matcher can end up without the label (e.g. a
	// config created by a partially-finished run or a manual helper), and if
	// we ignore it we'll try to recreate its version number and collide. By
	// keying on the baseName_vNNN naming scheme we stay pmcluster-owned even
	// when an orphan drops its label, and the orphan gets GC'd once a newer
	// version takes over and it's no longer mounted.
	existing, err := d.ConfigList(ctx, "", "")
	if err != nil {
		return "", false, fmt.Errorf("list configs: %w", err)
	}

	prefix := baseName + "_v"
	maxVer := 0
	for _, name := range existing {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		var v int
		if _, scanErr := fmt.Sscanf(name, prefix+"%d", &v); scanErr == nil && v > maxVer {
			maxVer = v
		}
	}

	// Content-aware: reuse the current highest version when its content
	// matches. Compare the pmcluster.data_hash label fingerprint rather than
	// the payload bytes — same reason as EnsureVersionedSecret: label-based
	// comparison works uniformly whether or not the API returns config data,
	// and keeps secrets/configs consistent.
	if maxVer > 0 {
		curName := fmt.Sprintf("%s_v%03d", baseName, maxVer)
		if cur, err := d.ConfigInspect(ctx, curName); err == nil {
			if h, ok := cur.Labels["pmcluster.data_hash"]; ok && h == dataHash(data) {
				return curName, false, nil
			}
		}
	}

	newVer := maxVer + 1
	versionedName = fmt.Sprintf("%s_v%03d", baseName, newVer)

	err = d.ConfigCreate(ctx, docker.ConfigSpec{
		Name: versionedName,
		Data: data,
		Labels: map[string]string{
			pmclusterLabel:        "true",
			"pmcluster.base":      baseName,
			"pmcluster.version":   version,
			"pmcluster.data_hash": dataHash(data),
		},
	})
	if err != nil {
		return "", false, fmt.Errorf("create config %s: %w", versionedName, err)
	}

	// Garbage-collect old versions (keep only the one we just created).
	for _, name := range existing {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if name == versionedName {
			continue
		}
		if rmErr := d.ConfigRemove(ctx, name); rmErr != nil {
			// Old config may still be in use; that's fine — it'll be
			// cleaned up on the next cluster up after stacks redeploy.
			// Don't fail the operation.
			_ = rmErr
		}
	}

	return versionedName, true, nil
}

// RandomPassword returns a cryptographically random password that meets
// OpenObserve complexity requirements: 8-128 characters with at least one
// lowercase letter, one uppercase letter, one digit, and one special character.
func RandomPassword() (string, error) {
	const (
		entropy      = 24
		special      = "!@#$%^&*"
		lowerLetters = "abcdefghijklmnopqrstuvwxyz"
		upperLetters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		digits       = "0123456789"
	)

	buf := make([]byte, entropy)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}

	// Base64-encode the random bytes, then append one guaranteed
	// character of each required type.  The result is ~38 chars and
	// always satisfies the OpenObserve password policy.
	base := base64.RawURLEncoding.EncodeToString(buf)

	// Pick one guaranteed char from each required set using the
	// first few entropy bytes (we already consumed buf, so use the
	// raw bytes directly before encoding for the indices).
	guaranteed := []byte{
		lowerLetters[int(buf[0])%len(lowerLetters)],
		upperLetters[int(buf[1])%len(upperLetters)],
		digits[int(buf[2])%len(digits)],
		special[int(buf[3])%len(special)],
	}

	// Shuffle the guaranteed chars into random positions in the base
	// string to avoid predictable placement.
	b := []byte(base)
	for i, gc := range guaranteed {
		pos := int(buf[4+i]) % len(b)
		b = append(b, 0)
		copy(b[pos+1:], b[pos:])
		b[pos] = gc
	}

	return string(b), nil
}

// HtpasswdLine produces "user:bcrypt-hash\n" for Traefik's basicAuth
// middleware. Cost 10 ≈ 100 ms/hash — defensive without slowing startup.
func HtpasswdLine(user, password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return "", fmt.Errorf("bcrypt: %w", err)
	}
	return fmt.Sprintf("%s:%s\n", user, hash), nil
}
