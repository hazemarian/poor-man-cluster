// Package backups — the platform export's drill.
//
// ADR 0007's artifact is checked for PRESENCE (`S3ObjectExists`) and by nothing else: a bucket
// that accepts the object proves it was written, not that it can be read back. That is the same
// gap the stack drill closed for volume archives — "an unverified backup is a hope" — and the
// platform's own restore point was holding itself to a lower standard than the stack artifacts.
//
// The drill answers three questions, in the order a restore would ask them:
//
//  1. does the manifest describe bytes that are actually in the archive (sha256 per member);
//  2. does data.db open, and does SQLite consider it intact (`PRAGMA integrity_check`);
//  3. does the archived key decrypt a secret from that database — the only step that proves the
//     key belongs to the ciphertext it was shipped with, which is the whole reason the export
//     carries both.
package backups

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the same pure-Go driver the store uses: no cgo, no build tag

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
)

// DrillReport is what the platform drill found.
type DrillReport struct {
	Archive    string
	ManifestAt string
	Hostname   string
	Version    string
	Members    int
	// IntegrityCheck is SQLite's own answer ("ok" when the file is sound).
	IntegrityCheck string
	// SecretsChecked / SecretsDecrypted prove the key opens the database's ciphertext.
	SecretsChecked   int
	SecretsDecrypted int
	// KeyProvesCiphertext is false when the archive carries no secrets at all: nothing to
	// disprove, and claiming the key was proven there would be a lie.
	KeyProvesCiphertext bool
	Duration            time.Duration
}

// OK reports whether the drill found a restorable archive.
func (r DrillReport) OK() bool {
	return r.IntegrityCheck == "ok" && (r.KeyProvesCiphertext || r.SecretsChecked == 0)
}

// NewestPlatformExport returns the newest local platform export in dir.
func NewestPlatformExport(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read archive dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, PlatformArchivePrefix+"-") || !strings.HasSuffix(n, ".tar.gz") {
			continue
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no %s-*.tar.gz in %s — take one with `pmcluster backup platform`", PlatformArchivePrefix, dir)
	}
	// Names carry a UTC timestamp, so lexical order is chronological.
	sort.Strings(names)
	return filepath.Join(dir, names[len(names)-1]), nil
}

// DrillPlatformExport unpacks an export into a scratch directory, checks it the way a restore
// would, and reports what it found. It never touches the live data dir.
func DrillPlatformExport(archivePath string) (DrillReport, error) {
	start := time.Now()
	rep := DrillReport{Archive: archivePath}

	source, err := os.Open(archivePath)
	if err != nil {
		return rep, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = source.Close() }()

	dir, err := os.MkdirTemp("", "pmcluster-platform-drill-*")
	if err != nil {
		return rep, fmt.Errorf("scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// (1) Unpack, hashing as we go, so the manifest can be compared with the bytes that landed.
	sums := map[string]string{}
	if err := unpackTo(source, dir, sums); err != nil {
		return rep, err
	}

	manifest, err := ReadPlatformManifest(archivePath)
	if err != nil {
		return rep, err
	}
	rep.ManifestAt = manifest.CreatedAt
	rep.Hostname = manifest.Hostname
	rep.Version = manifest.Version
	rep.Members = len(manifest.Members)
	if len(manifest.Members) == 0 {
		return rep, errors.New("the manifest lists no members: this archive does not describe itself")
	}
	for _, m := range manifest.Members {
		got, ok := sums[m.Name]
		if !ok {
			return rep, fmt.Errorf("the manifest lists %s but the archive does not contain it", m.Name)
		}
		if got != m.SHA256 {
			return rep, fmt.Errorf("%s does not match its manifest: sha256 %s, manifest says %s", m.Name, got, m.SHA256)
		}
	}

	// (2) The database: can it open, and does SQLite consider it sound?
	dbPath := filepath.Join(dir, "data.db")
	keyPath := filepath.Join(dir, ".encryption_key")
	if _, err := os.Stat(dbPath); err != nil {
		return rep, fmt.Errorf("no data.db in the archive: %w", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		return rep, fmt.Errorf("no .encryption_key in the archive: %w — a control-plane export without its key cannot decrypt a single secret", err)
	}
	integrity, payloads, err := inspectArchivedStore(dbPath)
	if err != nil {
		return rep, err
	}
	rep.IntegrityCheck = integrity
	rep.SecretsChecked = len(payloads)

	// (3) Does the archived key open that ciphertext?
	if len(payloads) > 0 {
		cipher, cerr := openArchivedKey(keyPath)
		if cerr != nil {
			return rep, cerr
		}
		for _, p := range payloads {
			if _, derr := cipher.Decrypt(p); derr != nil {
				return rep, fmt.Errorf("the archived key does not decrypt %d of %d sampled secret(s): the archive is not self-consistent", 1, len(payloads))
			}
			rep.SecretsDecrypted++
		}
		rep.KeyProvesCiphertext = true
	}
	rep.Duration = time.Since(start)
	return rep, nil
}

// unpackTo extracts the archive into dir and returns a sha256 per regular file.
func unpackTo(r io.Reader, dir string, sums map[string]string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Nothing outside the scratch dir: an archive naming ../ is not one of ours, and
		// writing where it says is how a restore becomes a compromise.
		name := filepath.Base(hdr.Name)
		if name == "." || name == string(filepath.Separator) || name == "" {
			continue
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(out, h), tr); err != nil {
			_ = out.Close()
			return fmt.Errorf("extract %s: %w", name, err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("close %s: %w", name, err)
		}
		sums[name] = hex.EncodeToString(h.Sum(nil))
	}
	return nil
}

// inspectArchivedStore runs SQLite's own integrity check and returns the ciphertexts of a
// bounded sample of secrets, so the key can be proved against them.
func inspectArchivedStore(dbPath string) (string, [][]byte, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return "", nil, fmt.Errorf("open the archived database: %w", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return "", nil, fmt.Errorf("integrity_check on the archived database: %w", err)
	}
	if integrity != "ok" {
		return integrity, nil, nil
	}

	rows, err := db.QueryContext(ctx, "SELECT payload FROM secrets LIMIT 4")
	if err != nil {
		// A store with no secrets table is still a valid control plane (nothing has been
		// created yet); the integrity check is all there is to prove.
		return integrity, nil, nil
	}
	defer func() { _ = rows.Close() }()
	var payloads [][]byte
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return integrity, nil, fmt.Errorf("read a secret from the archive: %w", err)
		}
		if len(p) > 0 {
			payloads = append(payloads, p)
		}
	}
	return integrity, payloads, nil
}

// openArchivedKey builds a cipher from the archived key file, through the same loader the daemon
// uses — a drill that decrypted with its own implementation would prove the wrong thing.
func openArchivedKey(keyPath string) (*credentials.Cipher, error) {
	c, err := credentials.OpenExisting(keyPath)
	if err != nil {
		return nil, fmt.Errorf("load the archived key: %w", err)
	}
	return c, nil
}
