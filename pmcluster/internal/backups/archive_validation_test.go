package backups

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEmptyArchive writes a valid tar.gz that holds no entries at all — the
// shape of a truncated artifact that used to parse as "nothing to restore".
func writeEmptyArchive(t *testing.T, dir, name string) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
}

// A file that is not an archive must be named as such, not reported as an
// archive with zero entries.
func TestListArchiveRefusesBytesThatAreNotAnArchive(t *testing.T) {
	cases := map[string]string{
		"an html error page": "<!DOCTYPE html><html><body>Access Denied</body></html>",
		"a json sidecar":     `{"artifact":"backup-x.tar.gz","size":1234}`,
		"a short text file":  "404",
		"an empty object":    "",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "backup-x.tar.gz")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			entries, err := listArchive(p)
			if err == nil {
				t.Fatalf("listArchive(%s) returned %d entries, want an error", name, len(entries))
			}
			if !errors.Is(err, ErrNotAnArchive) {
				t.Fatalf("error = %v, want it to wrap ErrNotAnArchive", err)
			}
		})
	}
}

// A valid archive with no entries is an error too: "no entries" must never be
// indistinguishable from "no files to restore".
func TestListArchiveRefusesAnArchiveWithNoEntries(t *testing.T) {
	dir := t.TempDir()
	writeEmptyArchive(t, dir, "empty.tar.gz")
	if _, err := listArchive(filepath.Join(dir, "empty.tar.gz")); err == nil {
		t.Fatal("listArchive accepted a zero-entry archive")
	}
}

// The exact failure this replaced: the object fetches with 200, the bytes are
// an error page, and the restore used to die inside gzip — or, with an empty
// body, "succeed" having restored nothing.
func TestRestoreFromS3RefusesAnObjectThatIsNotAnArchive(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"html page": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!DOCTYPE html><html><body>NoSuchKey</body></html>"))
		},
		"empty body": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := newTestStore(t)
			srv := httptest.NewServer(handler)
			defer srv.Close()

			old := s3HTTPClient
			s3HTTPClient = srv.Client()
			defer func() { s3HTTPClient = old }()

			archive := filepath.Join(t.TempDir(), "backup-node1-2026-11-05T10-00-00.tar.gz")
			id, _ := st.CreateBackup(context.Background(), "demo", 1)
			if err := st.FinishBackup(context.Background(), id, "succeeded", archive, ""); err != nil {
				t.Fatalf("FinishBackup: %v", err)
			}

			svc := &Local{Store: st, S3: S3Config{
				Endpoint: srv.URL, Bucket: "pmclusterbackup", AccessKey: "a", SecretKey: "s",
			}}
			_, err := svc.Restore(context.Background(), id, t.TempDir(), RestoreOptions{FromS3: true})
			if err == nil {
				t.Fatal("Restore accepted a 200 that was not an archive")
			}
			msg := err.Error()
			if name == "html page" {
				if !strings.Contains(msg, "not an archive") {
					t.Fatalf("error does not say the object is not an archive: %v", err)
				}
			}
			if !strings.Contains(msg, "pmclusterbackup") || !strings.Contains(msg, "backup-node1-2026-11-05T10-00-00.tar.gz") {
				t.Fatalf("error does not name the bucket and key: %v", err)
			}
		})
	}
}

// A 200 with no body is a fetch failure, not a zero-byte artifact.
func TestFetchS3ObjectRefusesAnEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	old := s3HTTPClient
	s3HTTPClient = srv.Client()
	defer func() { s3HTTPClient = old }()

	dst := filepath.Join(t.TempDir(), "out.tar.gz")
	err := fetchS3Object(context.Background(), S3Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
	}, "backup-x.tar.gz", dst)
	if err == nil {
		t.Fatal("fetchS3Object accepted a 200 with an empty body")
	}
	if !strings.Contains(err.Error(), "empty body") {
		t.Fatalf("error = %v, want it to say the body was empty", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("a partial file was left behind at %s", dst)
	}
}

// A body shorter than the announced length must fail the fetch and leave no
// partial file: a truncated download is not a smaller archive, it is garbage.
func TestFetchS3ObjectRefusesATruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte("only a fragment"))
	}))
	defer srv.Close()

	old := s3HTTPClient
	s3HTTPClient = srv.Client()
	defer func() { s3HTTPClient = old }()

	dst := filepath.Join(t.TempDir(), "out.tar.gz")
	err := fetchS3Object(context.Background(), S3Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
	}, "backup-x.tar.gz", dst)
	if err == nil {
		t.Fatal("fetchS3Object accepted a truncated body")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("a partial file was left behind at %s", dst)
	}
}

// A snapshot that captured an empty volume must say exactly that — not blame
// the restore scope, which sends the operator looking for a filter bug.
func TestRestoreEmptySnapshotSaysTheVolumeWasEmpty(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()
	writeTarGzDirs(t, dir, "backup-node1-2026-11-05T10-00-00.tar.gz", "backup/data", nil)
	archive := filepath.Join(dir, "backup-node1-2026-11-05T10-00-00.tar.gz")

	id, _ := st.CreateBackup(context.Background(), "demo", 1)
	if err := st.FinishBackup(context.Background(), id, "succeeded", archive, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	svc := &Local{Store: st}
	_, err := svc.Restore(context.Background(), id, t.TempDir(), RestoreOptions{})
	if err == nil {
		t.Fatal("Restore of an empty snapshot returned success")
	}
	if !strings.Contains(err.Error(), "EMPTY volume") {
		t.Fatalf("error does not say the snapshot was empty: %v", err)
	}
	if strings.Contains(err.Error(), "matched the restore scope") {
		t.Fatalf("error blames the restore scope for an empty snapshot: %v", err)
	}
}

// The other zero-file cause: the artifact has files, the scope excluded them.
func TestRestoreScopeMissNamesTheVolume(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()
	writeTarGzDirs(t, dir, "run.tar.gz", "backup/data", map[string]string{
		"demo/db_data/postgres/PG_VERSION": "16",
	})
	archive := filepath.Join(dir, "run.tar.gz")

	id, _ := st.CreateBackup(context.Background(), "demo", 1)
	if err := st.FinishBackup(context.Background(), id, "succeeded", archive, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	svc := &Local{Store: st}
	_, err := svc.Restore(context.Background(), id, t.TempDir(), RestoreOptions{Volume: "not_a_volume"})
	if err == nil {
		t.Fatal("Restore with a volume that matches nothing returned success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "none of which matched") || !strings.Contains(msg, "not_a_volume") {
		t.Fatalf("error does not explain the scope miss: %v", err)
	}
}
