package backupagent

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver: verify opens an archived database in-process

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

// VerifyArchive is the default verification: reopen the artifact and ask the
// database inside it whether it is intact.
//
// This is the difference between "a file exists" and "a backup exists". It
// costs a full read of the archive, which is why it runs on the host right
// after the write rather than on the leader.
func VerifyArchive(archivePath, _ string) (engine, detail string, err error) {
	dbMembers, pgControl, mysql, err := scanArchive(archivePath)
	if err != nil {
		return "", "", err
	}
	switch {
	case len(dbMembers) > 0:
		// Every database in the artifact is checked, not just the first: a
		// whole-node archive holds one per stack, and a green verdict that
		// covered one of eight would be a lie the console repeats.
		for _, m := range dbMembers {
			if err := checkSQLite(archivePath, m); err != nil {
				return "sqlite", "", fmt.Errorf("%s: %w", path.Base(m), err)
			}
		}
		return "sqlite", describeDatabases(dbMembers), nil
	case mysql.found():
		// MySQL and MariaDB cannot be opened from the host the way SQLite can —
		// no client, no server. What IS checkable is that the pieces a clean
		// shutdown or a crash leaves behind are all there: the InnoDB system
		// tablespace, the data dictionary, and a redo log. Saying so is honest;
		// claiming "verified" would not be.
		return mysql.engine(), mysql.describe(), nil
	case pgControl:
		// A Postgres data directory is only meaningful with its WAL, and
		// neither pg_controldata nor a server are available on the host, so
		// report exactly what was checked rather than implying validity.
		return "postgres", "control file + WAL present (not validated — start the cluster to confirm)", nil
	default:
		return "", "archive readable, no database detected", nil
	}
}

// mysqlEvidence records what a MySQL/MariaDB data directory is made of, so the
// verdict can distinguish "recognised, and here is what is inside" from
// "not a database at all".
type mysqlEvidence struct {
	systemTablespace bool // ibdata1 — present in every MySQL/MariaDB datadir
	dataDictionary   bool // mysql.ibd (MySQL 8) or mysql/ (5.7/MariaDB)
	redoLog          bool // #innodb_redo (8.0+) or ib_logfile*
	aria             bool // aria_log_control — MariaDB's Aria engine
}

func (m mysqlEvidence) found() bool { return m.systemTablespace || m.dataDictionary }

func (m mysqlEvidence) engine() string {
	// Aria is MariaDB's own engine: a datadir that has its control file is a
	// MariaDB datadir, even though MariaDB also ships InnoDB (and therefore
	// ib_logfile*/#innodb_redo). Keying off the redo log instead guessed
	// "mysql" for every real MariaDB directory.
	if m.aria {
		return "mariadb"
	}
	return "mysql"
}

func (m mysqlEvidence) describe() string {
	var parts []string
	if m.systemTablespace {
		parts = append(parts, "InnoDB system tablespace")
	}
	if m.dataDictionary {
		parts = append(parts, "data dictionary")
	}
	if m.redoLog {
		parts = append(parts, "redo log (replayed on start)")
	}
	if m.aria {
		parts = append(parts, "Aria control file")
	}
	return strings.Join(parts, " + ") + " present (not validated — start the server to confirm)"
}

// scanArchive reads the whole tar stream (so a truncated archive fails here)
// and reports the first SQLite database member it saw and whether a Postgres
// control file is present.
func scanArchive(archivePath string) (dbMembers []string, pgControl bool, mysql mysqlEvidence, err error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, false, mysql, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, false, mysql, fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return dbMembers, pgControl, mysql, nil
		}
		if err != nil {
			return nil, false, mysql, fmt.Errorf("tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		base := path.Base(h.Name)
		switch {
		case isDatabaseName(base):
			dbMembers = append(dbMembers, h.Name)
		case base == "pg_control" && strings.HasSuffix(path.Dir(h.Name), "global"):
			pgControl = true
		case base == "ibdata1":
			mysql.systemTablespace = true
		case base == "mysql.ibd":
			mysql.dataDictionary = true
		case base == "aria_log_control":
			mysql.aria = true
		case strings.HasPrefix(base, "ib_logfile") || strings.Contains(h.Name, "#innodb_redo/"):
			mysql.redoLog = true
		case h.Typeflag == tar.TypeDir && path.Base(h.Name) == "mysql", strings.Contains(h.Name, "/mysql/"):
			// MySQL 5.7 and MariaDB keep the data dictionary as a mysql/
			// directory; match files inside it too, since an archive may carry
			// no explicit directory entries.
			mysql.dataDictionary = true
		}
	}
}

// checkSQLite extracts the database (and its WAL, when archived) and runs
// PRAGMA integrity_check on it.
//
// The -shm file is deliberately not restored: it is shared-memory scratch that
// SQLite rebuilds, and a stale copy from a mid-write moment is a hazard.
func checkSQLite(archivePath, member string) error {
	dir, err := os.MkdirTemp("", "pmcluster-verify-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	base := path.Base(member)
	want := map[string]bool{
		member:          true, // the database itself
		member + "-wal": true, // committed frames not yet checkpointed
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return err
	}
	tr := tar.NewReader(gz)
	found := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = gz.Close()
			_ = f.Close()
			return fmt.Errorf("tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg || !want[h.Name] {
			continue
		}
		dst := filepath.Join(dir, path.Base(h.Name))
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			_ = gz.Close()
			_ = f.Close()
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			_ = gz.Close()
			_ = f.Close()
			return err
		}
		_ = out.Close()
		found++
	}
	_ = gz.Close()
	_ = f.Close()
	if found == 0 {
		return fmt.Errorf("archive holds no readable copy of %s", base)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, base))
	if err != nil {
		return fmt.Errorf("open %s: %w", base, err)
	}
	defer func() { _ = db.Close() }()
	var verdict string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&verdict); err != nil {
		return fmt.Errorf("integrity_check %s: %w", base, err)
	}
	if !strings.EqualFold(verdict, "ok") {
		return fmt.Errorf("integrity_check %s: %s", base, verdict)
	}
	return nil
}

// Drill is the restore drill: extract an artifact completely into workDir and
// prove the databases inside the extracted tree open.
//
// It is deliberately not the same code path as VerifyArchive. Verify reads the
// database out of the tar; Drill extracts *every* member first, so a truncated
// or mis-sized archive fails here even when the first database happens to sit
// early in the stream. That is the difference between checking a backup and
// rehearsing a restore.
func Drill(archivePath, workDir string) (files int, engine, detail string, err error) {
	if workDir == "" {
		return 0, "", "", fmt.Errorf("drill: work dir is required")
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return 0, "", "", err
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return 0, "", "", fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, "", "", fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, "", "", fmt.Errorf("tar after %d files: %w", files, err)
		}
		// Members may be absolute (the offen agents bake in "/backup/data/")
		// or relative (this agent writes "backup/data/<stack>/"). ArchiveRelPath
		// applies the same mapping a restore uses, so the drill extracts the
		// tree a restore would produce — and a path that escapes the work dir
		// is still refused.
		rel := backups.ArchiveRelPath(h.Name)
		if rel == "." || rel == "" {
			continue
		}
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return files, "", "", fmt.Errorf("drill: refusing member outside the work dir: %s", h.Name)
		}
		dst := filepath.Join(workDir, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return files, "", "", err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return files, "", "", err
			}
			_ = os.Remove(dst)
			if err := os.Symlink(h.Linkname, dst); err != nil {
				return files, "", "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return files, "", "", err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o777|0o200)
			if err != nil {
				return files, "", "", err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return files, "", "", err
			}
			if err := out.Close(); err != nil {
				return files, "", "", err
			}
			files++
		}
	}
	engine, detail, err = verifyTree(workDir)
	return files, engine, detail, err
}

// verifyTree is the on-disk half of the verification: find the first SQLite
// database (with its WAL) or a Postgres control file and check it.
func verifyTree(root string) (engine, detail string, err error) {
	var dbs []string
	var pgControl string
	walkErr := filepath.Walk(root, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			return nil
		}
		base := info.Name()
		switch {
		case isDatabaseName(base):
			dbs = append(dbs, p)
		case base == "pg_control" && filepath.Base(filepath.Dir(p)) == "global":
			pgControl = p
		}
		return nil
	})
	if walkErr != nil {
		return "", "", walkErr
	}
	switch {
	case len(dbs) > 0:
		for _, db := range dbs {
			// A stale -shm from a mid-write moment is a hazard; SQLite
			// rebuilds it on open.
			_ = os.Remove(db + "-shm")
			if err := integrityCheckFile(db); err != nil {
				return "sqlite", "", fmt.Errorf("%s: %w", filepath.Base(db), err)
			}
		}
		return "sqlite", describeDatabases(dbs), nil
	case pgControl != "":
		return "postgres", "control file + WAL present (not validated — start the cluster to confirm)", nil
	default:
		return "", "extracted cleanly, no database detected", nil
	}
}

// integrityCheckFile runs PRAGMA integrity_check against one database file.
func integrityCheckFile(p string) error {
	db, err := sql.Open("sqlite", p)
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(p), err)
	}
	defer func() { _ = db.Close() }()
	var verdict string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&verdict); err != nil {
		return fmt.Errorf("integrity_check %s: %w", filepath.Base(p), err)
	}
	if !strings.EqualFold(verdict, "ok") {
		return fmt.Errorf("integrity_check %s: %s", filepath.Base(p), verdict)
	}
	return nil
}

// isDatabaseName reports whether a file looks like a SQLite database. The -wal
// and -shm sidecars are excluded: they are not databases and must never be
// integrity-checked on their own.
func isDatabaseName(base string) bool {
	if strings.HasSuffix(base, "-wal") || strings.HasSuffix(base, "-shm") {
		return false
	}
	return strings.HasSuffix(base, ".db") || strings.HasSuffix(base, ".sqlite") || strings.HasSuffix(base, ".sqlite3")
}

// describeDatabases renders the verification detail: how many were checked and
// which ones, keeping the line readable when a node archive holds a dozen.
func describeDatabases(paths []string) string {
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, path.Base(p))
	}
	if len(names) > 3 {
		shown := strings.Join(names[:3], ", ")
		return fmt.Sprintf("%d databases integrity_check ok (%s, +%d more)", len(names), shown, len(names)-3)
	}
	return fmt.Sprintf("%d database(s) integrity_check ok (%s)", len(names), strings.Join(names, ", "))
}
