package backups

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The archive naming grammar.
//
// Two shapes live side by side so a cluster can migrate without losing its
// history:
//
//	backup-<nodeID>-<ts>.tar.gz                 whole-node archive (legacy, offen)
//	backup-<nodeID>-<ts>-stack-<stack>.tar.gz   one artifact per stack (agent)
//
// The legacy shape is parsed by code that predates the agent —
// archiveNodeID in internal/stacks takes everything between "backup-" and the
// next '-' as the node id — so the new shape keeps <nodeID> in that exact
// position and hangs the stack off a "-stack-" marker AFTER the timestamp.
// Both shapes therefore answer archiveNodeID identically, and the stack is
// unambiguous even though both node hostnames (nxt-sw-1-m) and stack names
// (khaal-pvc-cms) contain '-'.
const (
	// ArtifactTimeLayout matches the strftime tokens offen uses for
	// BACKUP_FILENAME (%Y-%m-%dT%H-%M-%S), so both producers sort the same way.
	ArtifactTimeLayout = "2006-01-02T15-04-05"
	// ArtifactPrefix is the prefix every volume archive carries. discoverStore
	// filters the object store on it, and stack move relies on it.
	ArtifactPrefix = "backup-"
	// ArtifactSuffix is the compressed-tar extension.
	ArtifactSuffix = ".tar.gz"
	// stackMarker separates the timestamp from the stack name in a per-stack
	// artifact. Nothing else in the grammar contains it.
	stackMarker = "-stack-"
)

// artifactRe parses both shapes. Node ids are alphanumeric (swarm ids contain
// none of "-", though hostnames used by older archives do), so the node is
// everything up to the 19-character timestamp, and the stack — when present —
// is whatever follows the marker.
var artifactRe = regexp.MustCompile(`^` + regexp.QuoteMeta(ArtifactPrefix) +
	`(?P<node>[A-Za-z0-9][A-Za-z0-9_.-]*?)-(?P<ts>\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2})` +
	`(?:` + regexp.QuoteMeta(stackMarker) + `(?P<stack>.+))?` + regexp.QuoteMeta(ArtifactSuffix) + `$`)

// Artifact is one volume archive on disk or in the object store.
type Artifact struct {
	// Stack is the app the artifact scopes to. Empty for a legacy whole-node
	// archive, which spans every stack that node held.
	Stack string
	// Node is the swarm node id (or hostname) of the producer.
	Node string
	// At is the timestamp baked into the name.
	At time.Time
	// Name is the filename / object key leaf.
	Name string
}

// ArtifactName renders the per-stack artifact name for a stack, node and time.
// Both node and stack are sanitised so a name can never introduce the marker
// or a path separator.
func ArtifactName(stack, node string, at time.Time) string {
	return ArtifactPrefix + sanitiseSegment(node) + "-" + at.UTC().Format(ArtifactTimeLayout) +
		stackMarker + sanitiseSegment(stack) + ArtifactSuffix
}

// sanitiseSegment strips anything that could break the grammar or a path: the
// marker substring, slashes, whitespace and the time separator.
func sanitiseSegment(s string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '-'
		}
	}, s)
	out = strings.ReplaceAll(out, stackMarker, "-")
	out = strings.Trim(out, "-")
	if out == "" {
		return "unknown"
	}
	return out
}

// ParseArtifact reports whether name is a volume archive and, when it is,
// which stack and node produced it. Legacy whole-node archives parse with an
// empty Stack.
func ParseArtifact(name string) (Artifact, bool) {
	name = filepath.Base(name)
	m := artifactRe.FindStringSubmatch(name)
	if m == nil {
		return Artifact{}, false
	}
	at, err := time.Parse(ArtifactTimeLayout, m[artifactRe.SubexpIndex("ts")])
	if err != nil {
		return Artifact{}, false
	}
	return Artifact{
		Stack: m[artifactRe.SubexpIndex("stack")],
		Node:  m[artifactRe.SubexpIndex("node")],
		At:    at.UTC(),
		Name:  name,
	}, true
}

// IsArtifact reports whether name looks like a volume archive at all. It is
// deliberately looser than ParseArtifact: the object store and the archive
// directory also hold control-plane archives (pmcluster-ctlplane-*) and
// purge backups, and those must not be indexed as volume data.
func IsArtifact(name string) bool {
	name = filepath.Base(name)
	return strings.HasPrefix(name, ArtifactPrefix) && strings.HasSuffix(name, ArtifactSuffix)
}

// ManifestName is the sidecar that records what an artifact holds. It sits
// next to the archive so discovery and verification can read it without
// untarring, and is ignored by every *.tar.gz scan.
func ManifestName(archiveName string) string { return archiveName + ".json" }

// Manifest is the sidecar written by the backup agent for one artifact.
//
// It is the machine-readable answer to "is this artifact trustworthy?" —
// which is the question the console, the CLI and the restore drill all have
// to answer, and which a bare tarball cannot.
type Manifest struct {
	// Stack and Node repeat the filename so a renamed archive is detectable.
	Stack string `json:"stack"`
	Node  string `json:"node"`
	// CreatedAt is the instant the snapshot was taken (unix seconds), not
	// the instant the tar finished.
	CreatedAt int64 `json:"created_at"`
	// Archive is the filename the manifest describes.
	Archive string `json:"archive"`
	// SizeBytes and SHA256 describe the archive file itself.
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	// VolumeRoot is the host path the snapshot was mounted from, and Prefix
	// is the member prefix inside the tar (always "backup/data/" so restore
	// and browse keep working unchanged).
	VolumeRoot string `json:"volume_root"`
	Prefix     string `json:"prefix"`
	// Files and Bytes count what went in.
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// Snapshot records how consistency was obtained.
	Snapshot SnapshotFacts `json:"snapshot"`
	// Verify is filled by the post-write check. Verified true means the
	// archive was reopened and its databases answered an integrity question.
	Verify VerifyFacts `json:"verify"`
	// Engine identifies the verification that ran, when one could: "sqlite",
	// "postgres", or "" when the stack had no recognisable database.
	Error string `json:"error,omitempty"`
}

// SnapshotFacts records the consistency mechanism behind one artifact.
type SnapshotFacts struct {
	// Kind is "lvm", "none" or "frozen" — none/frozen are used only by
	// dry runs and tests, never by a production run.
	Kind string `json:"kind"`
	// FrozenAt / ThawedAt bracket the write stall in unix nanoseconds.
	FrozenAt int64 `json:"frozen_at,omitempty"`
	ThawedAt int64 `json:"thawed_at,omitempty"`
	// LV is the snapshot device when Kind is "lvm".
	LV string `json:"lv,omitempty"`
	// PoolPercent is the thin-pool data percentage observed after the tars.
	PoolPercent float64 `json:"pool_percent,omitempty"`
}

// VerifyFacts is the outcome of reopening the archive.
type VerifyFacts struct {
	Verified bool   `json:"verified"`
	Engine   string `json:"engine,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Error    string `json:"error,omitempty"`
}

// WriteManifest writes the sidecar atomically next to archivePath.
func WriteManifest(archivePath string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	b = append(b, '\n')
	tmp := archivePath + ".json.tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(filepath.Dir(archivePath), ManifestName(filepath.Base(archivePath)))); err != nil {
		return fmt.Errorf("rename manifest: %w", err)
	}
	return nil
}

// ReadManifest reads the sidecar for archivePath. A missing sidecar is
// reported as os.ErrNotExist so callers can treat it as "unverified", not as
// a failure: archives written before the agent existed have none.
func ReadManifest(archivePath string) (Manifest, error) {
	p := filepath.Join(filepath.Dir(archivePath), ManifestName(filepath.Base(archivePath)))
	b, err := os.ReadFile(p)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %s: %w", p, err)
	}
	return m, nil
}

// artifactVerdictNote renders the sidecar's verification verdict as the note
// discovery stores on the row, so the console shows trust at a glance instead
// of an undifferentiated "succeeded". It returns "" when there is no sidecar
// (a legacy offen archive), which keeps that note empty as before.
func artifactVerdictNote(archivePath string) string {
	m, err := ReadManifest(archivePath)
	if err != nil {
		return ""
	}
	// An artifact whose manifest holds no files is a snapshot of an empty
	// volume. Saying so in the list is what stops "succeeded" from reading as
	// "your data is safe here" for a stack this node never held.
	if m.Files == 0 {
		return "empty: 0 files — the volume was empty on this node when the snapshot was taken"
	}
	switch {
	case m.Verify.Verified && m.Verify.Engine != "":
		return fmt.Sprintf("verified (%s): %s", m.Verify.Engine, m.Verify.Detail)
	case m.Verify.Verified:
		return "verified: " + m.Verify.Detail
	case m.Verify.Error != "":
		return "unverified: " + m.Verify.Error
	default:
		return "unverified"
	}
}

// ArchiveRelPath maps an archive member to a path relative to the restore
// target, applying the same rules the restore path uses. It exists so
// consumers outside this package (the backup agent's drill) cannot drift from
// what a restore would actually write.
func ArchiveRelPath(name string) string { return archiveRelPath(name) }
