package backups

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The sidecar is only worth shipping if something checks it. VerifyArchive is
// that check: it answers "are these the bytes that were taken?" — the one
// question an offsite copy cannot answer on its own.
func TestVerifyArchiveCatchesATamperedArtifact(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, ArtifactName("shop", "node1", time.Now().UTC()))
	if err := os.WriteFile(archive, []byte("the original bytes"), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	sum, err := SHA256File(archive)
	if err != nil {
		t.Fatalf("SHA256File: %v", err)
	}
	if err := WriteManifest(archive, Manifest{
		Stack: "shop", Node: "node1", Archive: filepath.Base(archive),
		SHA256: sum, SizeBytes: int64(len("the original bytes")),
	}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	checked, err := VerifyArchive(archive)
	if err != nil || !checked {
		t.Fatalf("VerifyArchive on a clean artifact: checked=%v err=%v", checked, err)
	}

	// A truncated download or an object overwritten in place.
	if err := os.WriteFile(archive, []byte("the orig"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	checked, err = VerifyArchive(archive)
	if !checked {
		t.Error("a tampered artifact reported as unchecked")
	}
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("VerifyArchive on a tampered artifact: err = %v, want ErrChecksumMismatch", err)
	}
}

// Artifacts written before sidecars existed must still restore: refusing them
// would strand exactly the history an operator reaches for.
func TestVerifyArchiveWithoutASidecarIsNotAFailure(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, ArtifactName("shop", "node1", time.Now().UTC()))
	if err := os.WriteFile(archive, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	checked, err := VerifyArchive(archive)
	if err != nil {
		t.Fatalf("no sidecar must not be an error: %v", err)
	}
	if checked {
		t.Error("reported a check without a manifest to check against")
	}
}
