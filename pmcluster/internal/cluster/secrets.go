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
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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

// EnsureVersionedSecretFromFile reads a file and provisions a content-addressed
// Swarm secret (e.g. cert_1a2b3c4d). Returns the versioned name and whether a
// new version was created (false when the file's bytes are unchanged and the
// current version is reused). hs records the data-hash of the minted version
// so the reuse check works despite Docker's write-only secret API.
func EnsureVersionedSecretFromFile(ctx context.Context, d runtime.Client, hs secretHashStore, baseName, path string) (versionedName string, created bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return EnsureVersionedSecret(ctx, d, hs, baseName, data)
}

// EnsureVersionedSecret provisions a content-addressed Swarm secret
// (e.g. cert_1a2b3c4d) and garbage-collects old versions. It is content-aware:
// if an object under this base already holds exactly these bytes, it is reused
// (created=false, no GC) so unchanged certs don't mint fresh objects. Same
// pattern as EnsureConfig.
//
// Because Docker secrets are write-only, "holds exactly these bytes" is
// decided by comparing dataHash(data) with the data-hash persisted in the DB
// (secretHashStore) when the current version was minted — never by inspecting
// the secret from Docker and never by trusting the pmcluster.data_hash label.
func EnsureVersionedSecret(ctx context.Context, d runtime.Client, hs secretHashStore, baseName string, data []byte) (versionedName string, created bool, err error) {
	// Content-addressed naming (shared rule, "no number added anymore"): the
	// object name is <baseName>_<sha8-of-data>. Identical content → identical
	// name → the existing object is reused as-is (created=false). A real
	// content change mints a brand-new name; the previous object is GC'd.
	// The stored DB hash is the source of truth for the reuse decision
	// (Docker's secret API is write-only, so the payload can never be read
	// back), mirroring the config path where the bytes are inspectable.
	stored := ""
	if hs != nil {
		stored = hs.GetSettingDefault(ctx, secretHashKey(baseName), "")
	}
	hash := dataHash(data)
	versionedName = store.SwarmSecretName(baseName, hash)

	// Reuse only when BOTH conditions hold: (1) the stored DB hash matches the
	// current content, AND (2) the content-addressed swarm object actually
	// exists. Condition (2) is the rebuild-on-missing half — after a naming
	// migration (e.g. old _v042 → content-addressed) the DB hash may match
	// unchanged content while the content-addressed object has never been
	// minted; reusing blindly would reference a non-existent secret and fail
	// the stack deploy (secret not found: cert_ab64977f).
	if stored == hash {
		exists, err := d.SecretExists(ctx, versionedName)
		if err != nil {
			return "", false, fmt.Errorf("inspect secret %s: %w", versionedName, err)
		}
		if exists {
			return versionedName, false, nil
		}
	}

	err = d.SecretCreate(ctx, runtime.SecretSpec{
		Name: versionedName,
		Data: data,
		Labels: map[string]string{
			pmclusterLabel:        "true",
			"pmcluster.base":      baseName,
			"pmcluster.data_hash": hash,
		},
	})
	if err != nil {
		return "", false, fmt.Errorf("create secret %s: %w", versionedName, err)
	}
	if hs != nil {
		if err := hs.SetSetting(ctx, secretHashKey(baseName), hash); err != nil {
			return "", false, fmt.Errorf("record secret data hash for %s: %w", baseName, err)
		}
	}

	// GC everything managed under this base except the freshly minted object.
	if err := gcManagedSecrets(ctx, d, baseName, versionedName); err != nil {
		_ = err
	}

	return versionedName, true, nil
}

// gcManagedSecrets removes every pmcluster-managed SECRET whose name is
// <baseName> or <baseName>_* (old content-addressed versions) except keepName.
// Best effort: individual removal failures are swallowed so a partial GC never
// blocks the caller.
func gcManagedSecrets(ctx context.Context, d runtime.Client, baseName, keepName string) error {
	prefix := baseName + "_"
	existing, err := d.SecretList(ctx, pmclusterLabel, "true")
	if err != nil {
		return fmt.Errorf("list secrets: %w", err)
	}
	for _, name := range existing {
		if name == keepName {
			continue
		}
		if name == baseName || strings.HasPrefix(name, prefix) {
			_ = d.SecretRemove(ctx, name)
		}
	}
	return nil
}

// gcManagedConfigs removes every pmcluster-managed CONFIG whose name is
// <baseName> or <baseName>_* (old content-addressed versions) except keepName.
// Best effort: individual removal failures are swallowed so a partial GC never
// blocks the caller.
func gcManagedConfigs(ctx context.Context, d runtime.Client, baseName, keepName string) error {
	prefix := baseName + "_"
	existing, err := d.ConfigList(ctx, pmclusterLabel, "true")
	if err != nil {
		return fmt.Errorf("list configs: %w", err)
	}
	for _, name := range existing {
		if name == keepName {
			continue
		}
		if name == baseName || strings.HasPrefix(name, prefix) {
			_ = d.ConfigRemove(ctx, name)
		}
	}
	return nil
}

// EnsureConfig is the Docker-config analogue of EnsureSecret. Docker configs
// are immutable, so we use a content-addressed name (<baseName>_<sha8-of-data>)
// and let the caller embed the versioned name into the compose file via the
// config_path()/ConfigNames resolver. Identical content → identical name → the
// existing object is reused as-is (created=false, no GC). A real content
// change mints a brand-new name and the previous object is GC'd. Unlike
// secrets, config bytes ARE inspectable from Docker, so the reuse decision
// compares the actual payload (never a stored hash and never the
// pmcluster.data_hash label).
//
// baseName is the logical name ("pmcluster_otel_config").
func EnsureConfig(ctx context.Context, d runtime.Client, baseName string, data []byte, version string) (versionedName string, created bool, err error) {
	hash := dataHash(data)
	versionedName = store.SwarmConfigName(baseName, hash)

	if cur, err := d.ConfigInspect(ctx, versionedName); err == nil && dataHash(cur.Data) == hash {
		return versionedName, false, nil
	}

	err = d.ConfigCreate(ctx, runtime.ConfigSpec{
		Name: versionedName,
		Data: data,
		Labels: map[string]string{
			pmclusterLabel:        "true",
			"pmcluster.base":      baseName,
			"pmcluster.version":   version,
			"pmcluster.data_hash": hash,
		},
	})
	if err != nil {
		return "", false, fmt.Errorf("create config %s: %w", versionedName, err)
	}

	// GC everything managed under this base except the freshly minted object.
	if err := gcManagedConfigs(ctx, d, baseName, versionedName); err != nil {
		_ = err
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
