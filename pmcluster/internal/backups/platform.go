// Package backups — manual platform export.
//
// The platform's control plane needs a restore point that survives losing EVERY manager.
// The mTLS mirror covers losing one: a follower already holds the document and can serve
// it. It does not cover all three disks dying, or an upgrade corrupting data.db fleet-wide,
// because then the mirror and the source die together.
//
// There is deliberately no scheduled service for this. A timer that archives the control
// plane reintroduces exactly what ADR 0005/0006 removed — an autonomous object in the
// cluster whose job is to move platform state around — and its restore path had no consumer
// after promotion stopped restoring from a kit. So the export is MANUAL and OPERATOR-DRIVEN:
// a button in the console (and `pmcluster backup platform`), run on the leader, written to
// the offsite target.
//
// The archive carries data.db AND .encryption_key together. That is a departure from the
// security split in ADR 0003, and it is the point of this artifact: a control-plane export
// without its key cannot decrypt a single stored secret, so it would look like a backup and
// restore into a cluster whose secrets are unreadable. ADR 0007 records why the coupling is
// acceptable here — a human initiates it, it is written once to an operator-owned bucket, and
// nothing in the platform ever reads it back automatically.
package backups

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// PlatformArchivePrefix names a manual control-plane export. The suffix is a UTC timestamp,
// so two exports sort chronologically by name.
const PlatformArchivePrefix = "pmcluster-platform"

// PlatformManifestName is the member that describes the archive to a human restoring it.
const PlatformManifestName = "MANIFEST.json"

// PlatformArchiveName is the file name for an export taken at now.
func PlatformArchiveName(now time.Time) string {
	return PlatformArchivePrefix + "-" + now.UTC().Format("20060102T150405Z") + ".tar.gz"
}

// PlatformMember is one file inside the archive.
type PlatformMember struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   string `json:"mode"`
}

// PlatformManifest is MANIFEST.json: what is inside, and what it was made from.
//
// It exists because a tarball in a bucket has no provenance: the operator about to restore
// needs to know which build wrote it, which host held it, and whether the bytes still match
// what was archived.
type PlatformManifest struct {
	Kind      string           `json:"kind"`
	CreatedAt string           `json:"created_at"`
	Hostname  string           `json:"hostname"`
	Version   string           `json:"version"`
	Commit    string           `json:"commit"`
	Members   []PlatformMember `json:"members"`
}

// PlatformExportOptions is what an export needs. Everything is explicit so the export is a
// pure function of its inputs — which is what makes it testable without a swarm.
type PlatformExportOptions struct {
	// DataDir holds data.db and .encryption_key.
	DataDir string
	// OutDir is where the archive is written (the node's backup archive directory).
	OutDir string
	// Hostname, Version and Commit go into the manifest.
	Hostname string
	Version  string
	Commit   string
	// Now is the clock; zero means time.Now().
	Now time.Time
}

// ErrNotLeader means the export was asked of a node that is not the swarm leader. Refusing
// is the point: a follower's data.db is a mirror of the leader's, so an export taken from it
// produces a restore point whose provenance nobody can name.
var ErrNotLeader = errors.New("the platform export runs on the swarm leader")

// PlatformExportResult is what a completed export hands back to a caller: the CLI prints it,
// the API route answers it.
type PlatformExportResult struct {
	Name       string           `json:"name"`
	Archive    string           `json:"archive"`
	Bytes      int64            `json:"bytes"`
	Members    []PlatformMember `json:"members"`
	OffsiteKey string           `json:"offsite_key"`
}

// PlatformExporter is the whole operation: check this node leads, export, ship it offsite.
//
// It lives here rather than in the CLI so the console button and `pmcluster backup platform`
// cannot drift: they are the same three steps with the same refusal.
type PlatformExporter struct {
	DataDir  string
	OutDir   string
	Hostname string
	Version  string
	Commit   string
	// Leader reports whether THIS node is the swarm leader. Nil means standalone: a box with
	// no swarm is its own leader.
	Leader func(ctx context.Context) (bool, error)
	// S3 resolves the offsite destination. Nil, or a config that is not Configured(), means
	// the archive stays local and the result says so instead of pretending it shipped.
	S3 func(ctx context.Context) S3Config
	// LocalOnly skips the upload (the CLI flag). The console button always uploads.
	LocalOnly bool
}

// Run performs the export. A nil error with an empty OffsiteKey means it was written locally
// and no offsite destination was configured — reported rather than hidden, because an export
// that only exists on the host it backs up is the failure mode this artifact exists to avoid.
func (e PlatformExporter) Run(ctx context.Context) (PlatformExportResult, error) {
	if e.Leader != nil {
		leader, err := e.Leader(ctx)
		if err != nil {
			return PlatformExportResult{}, fmt.Errorf("ask swarm who leads: %w", err)
		}
		if !leader {
			return PlatformExportResult{}, ErrNotLeader
		}
	}
	res, err := ExportPlatform(PlatformExportOptions{
		DataDir: e.DataDir, OutDir: e.OutDir,
		Hostname: e.Hostname, Version: e.Version, Commit: e.Commit,
	})
	if err != nil {
		return PlatformExportResult{}, err
	}
	out := PlatformExportResult{
		Name: res.Name, Archive: res.Path, Bytes: res.Bytes, Members: res.Members,
	}
	if e.LocalOnly || e.S3 == nil {
		return out, nil
	}
	cfg := e.S3(ctx)
	if !cfg.Configured() {
		return out, nil
	}
	if err := PutS3Object(ctx, cfg, res.Name, res.Path); err != nil {
		return out, fmt.Errorf("upload %s offsite: %w", res.Name, err)
	}
	out.OffsiteKey = res.Name
	return out, nil
}

// PlatformExport is the result of a successful export.
type PlatformExport struct {
	Path    string
	Bytes   int64
	Name    string
	Members []PlatformMember
}

// ExportPlatform writes the leading control plane to OutDir as a gzip tar: data.db, the
// encryption key, and a manifest.
//
// It REFUSES to write a partial archive. A missing key means the result could not decrypt the
// secrets it claims to protect, so an export without it is a misleading artifact rather than
// a degraded one — the operator would discover that only during the restore.
func ExportPlatform(opts PlatformExportOptions) (PlatformExport, error) {
	if opts.DataDir == "" || opts.OutDir == "" {
		return PlatformExport{}, fmt.Errorf("export platform: data dir and out dir are required")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	dbPath := filepath.Join(opts.DataDir, "data.db")
	keyPath := filepath.Join(opts.DataDir, ".encryption_key")

	for _, f := range []string{dbPath, keyPath} {
		if _, err := os.Stat(f); err != nil {
			return PlatformExport{}, fmt.Errorf("export platform: %s: %w — refusing to write an archive that cannot restore what it claims to", f, err)
		}
	}
	if err := os.MkdirAll(opts.OutDir, 0o700); err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: create archive dir: %w", err)
	}

	name := PlatformArchiveName(now)
	// Write to a temp name first: a half-written .tar.gz in the archive directory is an
	// artifact `backup list` and a human both read as complete.
	tmp, err := os.CreateTemp(opts.OutDir, ".platform-export-*.part")
	if err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op once renamed
	}()

	var members []PlatformMember
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)

	add := func(src, nameInArchive string) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:    nameInArchive,
			Mode:    int64(st.Mode().Perm()),
			Size:    st.Size(),
			ModTime: st.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		// One pass: the bytes are hashed as they are copied, so the manifest describes
		// exactly what landed in the archive rather than what was on disk beforehand.
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(tw, h), f)
		if err != nil {
			return err
		}
		members = append(members, PlatformMember{
			Name:   nameInArchive,
			SHA256: hex.EncodeToString(h.Sum(nil)),
			Bytes:  n,
			Mode:   fmt.Sprintf("%04o", st.Mode().Perm()),
		})
		return nil
	}

	// data.db first, key second: a restore reads them in that order.
	if err := add(dbPath, "data.db"); err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: add data.db: %w", err)
	}
	if err := add(keyPath, ".encryption_key"); err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: add encryption key: %w", err)
	}

	manifest := PlatformManifest{
		Kind:      "pmcluster-platform-export",
		CreatedAt: now.UTC().Format(time.RFC3339),
		Hostname:  opts.Hostname,
		Version:   opts.Version,
		Commit:    opts.Commit,
		Members:   members,
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: manifest: %w", err)
	}
	body = append(body, '\n')
	if err := tw.WriteHeader(&tar.Header{
		Name:    PlatformManifestName,
		Mode:    0o600,
		Size:    int64(len(body)),
		ModTime: now,
	}); err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: manifest header: %w", err)
	}
	if _, err := tw.Write(body); err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: write manifest: %w", err)
	}

	if err := tw.Close(); err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: close gzip: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: close temp: %w", err)
	}

	final := filepath.Join(opts.OutDir, name)
	if err := os.Rename(tmpName, final); err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: rename into place: %w", err)
	}
	if err := os.Chmod(final, 0o600); err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: chmod archive: %w", err)
	}
	st, err := os.Stat(final)
	if err != nil {
		return PlatformExport{}, fmt.Errorf("export platform: stat archive: %w", err)
	}
	return PlatformExport{Path: final, Name: name, Bytes: st.Size(), Members: members}, nil
}

// ReadPlatformManifest reads MANIFEST.json out of an export, so a restore can be checked
// before anything is unpacked over a live data dir. Bounded to a sane size: it is a small
// JSON file, and an archive claiming otherwise is not one of ours.
func ReadPlatformManifest(archivePath string) (PlatformManifest, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return PlatformManifest{}, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return PlatformManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	const limit = 1 << 20
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return PlatformManifest{}, fmt.Errorf("read manifest: %w", err)
		}
		if hdr.Name != PlatformManifestName {
			continue
		}
		if hdr.Size > limit {
			return PlatformManifest{}, fmt.Errorf("read manifest: %s is %d bytes, which is not a manifest", PlatformManifestName, hdr.Size)
		}
		var m PlatformManifest
		if err := json.NewDecoder(io.LimitReader(tr, limit)).Decode(&m); err != nil {
			return PlatformManifest{}, fmt.Errorf("read manifest: %w", err)
		}
		return m, nil
	}
	return PlatformManifest{}, fmt.Errorf("read manifest: %s is not in %s", PlatformManifestName, archivePath)
}
