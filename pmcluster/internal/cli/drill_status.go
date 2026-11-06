package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

// drillRecord is one completed drill, as the weekly timer leaves it behind.
type drillRecord struct {
	At      time.Time `json:"at"`
	Archive string    `json:"archive"`
	Files   int       `json:"files"`
	Engine  string    `json:"engine,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
}

// drillStatusFile is what `backup-agent drill-status` reads: the last drill and
// a short history, so "has anything actually been restored this month?" has an
// answer that does not depend on somebody's memory or a journal grep.
type drillStatusFile struct {
	Last    *drillRecord  `json:"last,omitempty"`
	History []drillRecord `json:"history,omitempty"`
}

const drillHistoryKeep = 12

func drillStatusPath(stateDir string) string {
	return filepath.Join(stateDir, "drill", "status.json")
}

// recordDrill appends one drill result and keeps the last N.
func recordDrill(stateDir string, rec drillRecord) error {
	if stateDir == "" {
		return nil
	}
	p := drillStatusPath(stateDir)
	var doc drillStatusFile
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &doc)
	}
	doc.Last = &rec
	doc.History = append([]drillRecord{rec}, doc.History...)
	if len(doc.History) > drillHistoryKeep {
		doc.History = doc.History[:drillHistoryKeep]
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o600)
}

// newestArtifacts returns up to n artifacts from dir, newest first. Sidecars and
// anything that is not a backup artifact are skipped.
func newestArtifacts(dir string, n int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) == ".json" {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if !backups.IsArtifact(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		cands = append(cands, cand{path: full, mod: fi.ModTime()})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	if n > len(cands) {
		n = len(cands)
	}
	out := make([]string, 0, n)
	for _, c := range cands[:n] {
		out = append(out, c.path)
	}
	return out, nil
}

// ensureDrillSpace refuses a drill the filesystem cannot hold.
//
// A drill extracts the whole artifact, so it needs the archive's size a second
// time, plus the headroom a verify pass uses. Filling the disk that a backup run
// just freed (or the volume the app writes to) turns a safety check into an
// incident, so this is a refusal with a number in it, not a warning.
func ensureDrillSpace(workDir string, archiveBytes int64) error {
	if archiveBytes <= 0 {
		return nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(workDir, &st); err != nil {
		return nil // cannot measure: do not block the drill on it
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	need := archiveBytes*2 + (256 << 20)
	if free < need {
		return fmt.Errorf("not enough space under %s: the drill needs %s (the archive again plus 256 MiB headroom), %s is free — free space or point --work-dir at a larger filesystem",
			workDir, humanBytes(need), humanBytes(free))
	}
	return nil
}
