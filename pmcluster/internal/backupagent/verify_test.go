package backupagent

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTar builds an archive from member → contents, the shape an artifact has.
func makeTar(t *testing.T, members map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range members {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// A MySQL/MariaDB data directory cannot be opened from the host, but it can be
// recognised — and "recognised, and here is what is inside" is a very different
// report from the empty engine this produced before.
func TestVerifyRecognisesMariaDB(t *testing.T) {
	archive := makeTar(t, map[string]string{
		"backup/data/mysql-app/db/ibdata1":           "innodb",
		"backup/data/mysql-app/db/mysql/user.frm":    "dict",
		"backup/data/mysql-app/db/ib_logfile0":       "redo",
		"backup/data/mysql-app/db/aria_log_control":  "aria",
		"backup/data/mysql-app/db/events/events.ibd": "table",
	})
	engine, detail, err := VerifyArchive(archive, "mysql-app")
	if err != nil {
		t.Fatalf("VerifyArchive: %v", err)
	}
	if engine != "mariadb" {
		t.Fatalf("engine = %q, want mariadb", engine)
	}
	for _, want := range []string{"InnoDB system tablespace", "data dictionary", "redo log", "Aria"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q does not mention %q", detail, want)
		}
	}
	if strings.Contains(detail, "verified") {
		t.Fatalf("detail overclaims: %q", detail)
	}
}

func TestVerifyRecognisesMySQL8(t *testing.T) {
	archive := makeTar(t, map[string]string{
		"backup/data/mysql-app/db/ibdata1":                 "innodb",
		"backup/data/mysql-app/db/mysql.ibd":               "dict",
		"backup/data/mysql-app/db/#innodb_redo/#ib_redo10": "redo",
	})
	engine, detail, err := VerifyArchive(archive, "mysql-app")
	if err != nil {
		t.Fatalf("VerifyArchive: %v", err)
	}
	if engine != "mysql" {
		t.Fatalf("engine = %q, want mysql", engine)
	}
	if !strings.Contains(detail, "data dictionary") || !strings.Contains(detail, "redo log") {
		t.Fatalf("detail = %q", detail)
	}
}
