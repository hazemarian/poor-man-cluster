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

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"golang.org/x/crypto/bcrypt"
)

// dataHash returns the stable hex sha256 of a config/secret's payload, used
// as a content fingerprint. Versioned configs/secrets are compared by hashing
// the actual bytes (never by trusting the pmcluster.data_hash label).
func dataHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// secretHashStore persists the data-hash of the current versioned secret.
// Docker's secret API is write-only — SecretInspect never returns the payload
// (neither a real daemon nor a fake can read it back) — so the reuse check
// cannot hash Docker's view of the bytes. Instead the hash is stored in the
// DB when a version is minted and compared against on subsequent runs. This
// is the same "hash the data, compare with the stored hash" rule applied to
// configs, with the DB as the source of truth.
type secretHashStore interface {
	GetSettingDefault(ctx context.Context, key, fallback string) string
	SetSetting(ctx context.Context, key, value string) error
}

// secretHashKey returns the settings key that holds the data-hash of the
// current version of baseName's versioned secret.
func secretHashKey(baseName string) string {
	return "secret_data_hash:" + baseName
}

// EnsureSecret creates a Swarm secret if one with this name doesn't
// exist; returns true iff newly created. Existing secrets are NEVER
// modified — rotation is "remove + recreate + force-redeploy", handled
// by CredentialsManager.Rotate.
//
// pmcluster-managed secrets carry the pmclusterLabel so `cluster down
// --purge` can target them without touching operator-created secrets.
func EnsureSecret(ctx context.Context, d runtime.Client, name string, data []byte) (created bool, err error) {
	exists, err := d.SecretExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("check secret %s: %w", name, err)
	}
	if exists {
		return false, nil
	}
	err = d.SecretCreate(ctx, runtime.SecretSpec{
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

// EnsureSecretFromFile creates or refreshes a Docker secret whose payload is
// read from path, returning whether the secret was (re)created.
func EnsureSecretFromFile(ctx context.Context, d runtime.Client, name, path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return EnsureSecret(ctx, d, name, data)
}

// EnsureVersionedSecretFromFile reads a file and provisions a versioned
// Swarm secret (e.g. cert_v001, cert_v002). Returns the versioned name and
// whether a new version was created (false when the file's bytes are unchanged
// and the current version is reused). hs records the data-hash of the minted
// version so the reuse check works despite Docker's write-only secret API.
func EnsureVersionedSecretFromFile(ctx context.Context, d runtime.Client, hs secretHashStore, baseName, path string) (versionedName string, created bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return EnsureVersionedSecret(ctx, d, hs, baseName, data)
}

// EnsureVersionedSecret provisions a versioned Swarm secret (e.g. cert_v001)
// and garbage-collects old versions. It is content-aware: if the current
// highest version already holds exactly these bytes, that version is reused
// (created=false, no GC) so unchanged certs don't mint fresh versions. Same
// pattern as EnsureConfig.
//
// Because Docker secrets are write-only, "holds exactly these bytes" is
// decided by comparing dataHash(data) with the data-hash persisted in the DB
// (secretHashStore) when the current version was minted — never by inspecting
// the secret from Docker and never by trusting the pmcluster.data_hash label.
func EnsureVersionedSecret(ctx context.Context, d runtime.Client, hs secretHashStore, baseName string, data []byte) (versionedName string, created bool, err error) {
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

	if maxVer > 0 {
		curName := fmt.Sprintf("%s_v%03d", baseName, maxVer)
		stored := ""
		if hs != nil {
			stored = hs.GetSettingDefault(ctx, secretHashKey(baseName), "")
		}
		// Content is compared by hashing the actual bytes and checking against
		// the stored DB hash — never by trusting the pmcluster.data_hash label
		// and never by reading the secret back from Docker (write-only API).
		if stored != "" && stored == dataHash(data) {
			return curName, false, nil
		}
	}

	newVer := maxVer + 1
	versionedName = fmt.Sprintf("%s_v%03d", baseName, newVer)

	err = d.SecretCreate(ctx, runtime.SecretSpec{
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
	if hs != nil {
		if err := hs.SetSetting(ctx, secretHashKey(baseName), dataHash(data)); err != nil {
			return "", false, fmt.Errorf("record secret data hash for %s: %w", baseName, err)
		}
	}

	for _, name := range existing {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if name == versionedName {
			continue
		}
		if rmErr := d.SecretRemove(ctx, name); rmErr != nil {
			_ = rmErr
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
func EnsureConfig(ctx context.Context, d runtime.Client, baseName string, data []byte, version string) (versionedName string, created bool, err error) {

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

	if maxVer > 0 {
		curName := fmt.Sprintf("%s_v%03d", baseName, maxVer)
		if cur, err := d.ConfigInspect(ctx, curName); err == nil {
			// Content is compared by hashing the actual config bytes, never by
			// trusting the pmcluster.data_hash label — the label could be
			// stale, missing, or wrong, and the bytes are the source of truth.
			if dataHash(cur.Data) == dataHash(data) {
				return curName, false, nil
			}
		}
	}

	newVer := maxVer + 1
	versionedName = fmt.Sprintf("%s_v%03d", baseName, newVer)

	err = d.ConfigCreate(ctx, runtime.ConfigSpec{
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

	for _, name := range existing {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if name == versionedName {
			continue
		}
		if rmErr := d.ConfigRemove(ctx, name); rmErr != nil {

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

	base := base64.RawURLEncoding.EncodeToString(buf)

	guaranteed := []byte{
		lowerLetters[int(buf[0])%len(lowerLetters)],
		upperLetters[int(buf[1])%len(upperLetters)],
		digits[int(buf[2])%len(digits)],
		special[int(buf[3])%len(special)],
	}

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
