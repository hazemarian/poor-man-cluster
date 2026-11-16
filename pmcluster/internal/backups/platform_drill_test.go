package backups

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// platformWithSecret builds a data dir that looks like a live control plane: a database with one
// encrypted secret, and the key that encrypts it.
func platformWithSecret(t *testing.T, value string) (dataDir string) {
	t.Helper()
	ctx := context.Background()
	dataDir = t.TempDir()
	keyPath := filepath.Join(dataDir, ".encryption_key")
	// Mint the key first (a fresh node's init), then encrypt with it.
	minted, err := credentials.Open(keyPath)
	if err != nil {
		t.Fatalf("mint key: %v", err)
	}
	st, err := store.Open(filepath.Join(dataDir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	payload, err := minted.Encrypt([]byte(value))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := st.CreateSecret(ctx, "cluster", "", "offsite-key", payload, store.SecretHash(value)); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return dataDir
}

// TestDrillPlatformExport_ProvesARestorableArchive is the whole point: manifest, integrity and
// key-versus-ciphertext, on a real export.
func TestDrillPlatformExport_ProvesARestorableArchive(t *testing.T) {
	dataDir := platformWithSecret(t, "s3cr3t-value")
	out := t.TempDir()
	res, err := ExportPlatform(PlatformExportOptions{
		DataDir: dataDir, OutDir: out, Hostname: "nxt-sw-5-m",
		Version: "v0.2.355", Commit: "abc1234",
		Now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ExportPlatform: %v", err)
	}

	rep, err := DrillPlatformExport(res.Path)
	if err != nil {
		t.Fatalf("DrillPlatformExport: %v", err)
	}
	if rep.IntegrityCheck != "ok" {
		t.Fatalf("integrity_check = %q, want ok", rep.IntegrityCheck)
	}
	if rep.Members != 2 {
		t.Errorf("members = %d, want 2 (data.db + key)", rep.Members)
	}
	if rep.Hostname != "nxt-sw-5-m" || rep.Version != "v0.2.355" {
		t.Errorf("drill lost the provenance: %+v", rep)
	}
	if rep.SecretsChecked == 0 || rep.SecretsDecrypted != rep.SecretsChecked {
		t.Errorf("secrets: checked %d, decrypted %d — the drill must prove the key opens them", rep.SecretsChecked, rep.SecretsDecrypted)
	}
	if !rep.KeyProvesCiphertext || !rep.OK() {
		t.Errorf("OK() = %v with KeyProvesCiphertext = %v", rep.OK(), rep.KeyProvesCiphertext)
	}
}

// TestDrillPlatformExport_RejectsATamperedArchive: a byte flipped inside the tarball must fail the
// manifest comparison, not slip through as "present".
func TestDrillPlatformExport_RejectsATamperedArchive(t *testing.T) {
	dataDir := platformWithSecret(t, "s3cr3t-value")
	out := t.TempDir()
	res, err := ExportPlatform(PlatformExportOptions{DataDir: dataDir, OutDir: out, Hostname: "h"})
	if err != nil {
		t.Fatalf("ExportPlatform: %v", err)
	}
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the middle of the gzip stream's payload region.
	mid := len(body) / 2
	body[mid] ^= 0xFF
	tampered := filepath.Join(out, "tampered.tar.gz")
	if err := os.WriteFile(tampered, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DrillPlatformExport(tampered); err == nil {
		t.Fatal("the drill accepted a tampered archive")
	}
}

// TestDrillPlatformExport_RefusesAnExportWithoutItsKey: the key is what makes the artifact worth
// having; without it the archive cannot decrypt the secrets it carries.
func TestDrillPlatformExport_RefusesAnExportWithoutItsKey(t *testing.T) {
	dataDir := platformWithSecret(t, "s3cr3t-value")
	out := t.TempDir()
	res, err := ExportPlatform(PlatformExportOptions{DataDir: dataDir, OutDir: out, Hostname: "h"})
	if err != nil {
		t.Fatalf("ExportPlatform: %v", err)
	}
	// Rebuild an archive that has data.db and a manifest but no key, by exporting then trimming.
	stripped := filepath.Join(out, "stripped.tar.gz")
	if err := rewriteArchiveWithout(res.Path, stripped, ".encryption_key"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	_, err = DrillPlatformExport(stripped)
	if err == nil {
		t.Fatal("the drill accepted an export with no encryption key")
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("the failure does not name the missing key: %v", err)
	}
}

// rewriteArchiveWithout copies an export minus one member, so the "no key" case can be built.
func rewriteArchiveWithout(src, dst, drop string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	gzIn, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer func() { _ = gzIn.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	gzOut := gzip.NewWriter(out)
	tw := tar.NewWriter(gzOut)
	tr := tar.NewReader(gzIn)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Name == drop {
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gzOut.Close()
}

func TestNewestPlatformExport(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"pmcluster-platform-20260101T000000Z.tar.gz",
		"pmcluster-platform-20260301T000000Z.tar.gz",
		"backup-node-20260101T000000Z-stack-shop.tar.gz",
		"pmcluster-platform-20260201T000000Z.tar.gz.json",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := NewestPlatformExport(dir)
	if err != nil {
		t.Fatalf("NewestPlatformExport: %v", err)
	}
	if filepath.Base(got) != "pmcluster-platform-20260301T000000Z.tar.gz" {
		t.Errorf("newest = %s, want the 2026-03-01 export", filepath.Base(got))
	}

	if _, err := NewestPlatformExport(t.TempDir()); err == nil {
		t.Error("an empty archive dir did not error")
	}
}
