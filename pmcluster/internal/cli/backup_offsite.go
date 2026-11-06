package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

// offsiteCheckRecord is one completed offsite check.
//
// The hourly run records where an artifact went ("s3:bucket/name") at the time
// it shipped. That is evidence of intent, not of presence: buckets have
// lifecycle rules, quotas, expiring keys, and someone can delete a bucket. This
// is the check that answers "is yesterday's backup still THERE", which is the
// only question that matters when the local copy is gone.
type offsiteCheckRecord struct {
	At         time.Time `json:"at"`
	Target     string    `json:"target"` // label + bucket
	Checked    int       `json:"checked"`
	Present    int       `json:"present"`
	Missing    []string  `json:"missing,omitempty"`
	NoManifest []string  `json:"no_manifest,omitempty"`
	// Sample is a few keys the bucket actually holds (newest first). A miss is
	// only actionable next to the bucket's own contents: a prefix, the wrong
	// bucket, an empty store and a lost object all look identical from the
	// missing-name list alone.
	Sample []string `json:"sample,omitempty"`
	// Objects is how many keys the target listing returned. A "missing"
	// artifact next to a tiny number is a listing problem, not a lost backup —
	// the distinction the first version of this check could not make.
	Objects int    `json:"objects"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// offsiteStatusFile is what `backup-agent offsite-status` reads.
type offsiteStatusFile struct {
	Last    *offsiteCheckRecord  `json:"last,omitempty"`
	History []offsiteCheckRecord `json:"history,omitempty"`
}

const offsiteHistoryKeep = 14

func offsiteStatusPath(stateDir string) string {
	return filepath.Join(stateDir, "offsite", "status.json")
}

func recordOffsiteCheck(stateDir string, rec offsiteCheckRecord) error {
	if stateDir == "" {
		return nil
	}
	p := offsiteStatusPath(stateDir)
	var doc offsiteStatusFile
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &doc)
	}
	doc.Last = &rec
	doc.History = append([]offsiteCheckRecord{rec}, doc.History...)
	if len(doc.History) > offsiteHistoryKeep {
		doc.History = doc.History[:offsiteHistoryKeep]
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

var backupAgentVerifyOffsiteCmd = &cobra.Command{
	Use:   "verify-offsite",
	Short: "Check the offsite bucket still holds the artifacts this node wrote",
	Long: `Ships happen hourly and the run record says where each artifact went. None of
that proves the object is still there tomorrow: S3 lifecycle rules expire
objects, quotas fill, credentials rotate, and a bucket can be deleted.

This lists the last N days of local artifacts (--days, default 1), checks each
archive AND its .json manifest sidecar against every configured target, and
records the result where ` + "`offsite-status`" + ` and the console can read it.

A manifest matters as much as the archive: the sidecar carries the sha256 that
restore verifies against, so a bucket with archives and no manifests can be
restored only blindly.`,
	Args: cobra.NoArgs,
	RunE: runVerifyOffsite,
}

func init() {
	backupAgentVerifyOffsiteCmd.Flags().Int("days", 1, "how many days of local artifacts to check")
	backupAgentVerifyOffsiteCmd.Flags().Bool("json", false, "machine-readable output")
	backupAgentVerifyOffsiteCmd.Flags().Bool("list-keys", false, "print every object key in each target (diagnostics)")
}

func runVerifyOffsite(cmd *cobra.Command, _ []string) error {
	days, _ := cmd.Flags().GetInt("days")
	if days < 1 {
		days = 1
	}
	ctx := cmd.Context()
	archiveDir := envOr("PMCLUSTER_BACKUP_DIR", "/var/stack/backup")
	stateDir := envOr("PMCLUSTER_BACKUP_STATE", "/var/lib/pmcluster-backup")

	locals, err := recentArtifacts(archiveDir, days)
	if err != nil {
		return err
	}

	targets := backupShipTargets()
	out := cmd.OutOrStdout()
	asJSON, _ := cmd.Flags().GetBool("json")

	if len(targets) == 0 {
		msg := fmt.Sprintf("no offsite target configured on this node (%d local artifact(s) in the last %d day(s)) — local only", len(locals), days)
		fmt.Fprintln(out, msg)
		_ = recordOffsiteCheck(stateDir, offsiteCheckRecord{
			At: time.Now().UTC(), Target: "(none)", Checked: len(locals),
			Error: "no offsite target configured",
		})
		return nil
	}

	if listKeys, _ := cmd.Flags().GetBool("list-keys"); listKeys {
		for _, t := range backupShipTargets() {
			objs, lerr := backups.ListS3Objects(ctx, t.S3, "")
			fmt.Fprintf(out, "== %s:%s (%d object(s))\n", t.Label, t.S3.Bucket, len(objs))
			if lerr != nil {
				fmt.Fprintf(out, "   error: %v\n", lerr)
				continue
			}
			for _, o := range objs {
				fmt.Fprintln(out, "   "+o.Key)
			}
		}
		return nil
	}

	failed := false
	var recs []offsiteCheckRecord
	for _, t := range targets {
		rec := offsiteCheckRecord{
			At:     time.Now().UTC(),
			Target: fmt.Sprintf("%s:%s", t.Label, t.S3.Bucket),
		}
		objects, lerr := backups.ListS3Objects(ctx, t.S3, "")
		if lerr != nil {
			rec.Error = lerr.Error()
			rec.OK = false
			failed = true
			recs = append(recs, rec)
			continue
		}
		present := make(map[string]bool, len(objects))
		for _, o := range objects {
			present[o.Key] = true
		}
		rec.Objects = len(objects)
		keys := make([]string, 0, len(objects))
		for _, o := range objects {
			keys = append(keys, o.Key)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		if len(keys) > 5 {
			keys = keys[:5]
		}
		rec.Sample = keys
		for _, local := range locals {
			rec.Checked++
			name := filepath.Base(local)
			if !present[name] {
				rec.Missing = append(rec.Missing, name)
				continue
			}
			rec.Present++
			// The sidecar is what makes a restore verifiable rather than
			// hopeful; its absence is reported even though the archive is there.
			if !present[backups.ManifestName(name)] {
				rec.NoManifest = append(rec.NoManifest, name)
			}
		}
		rec.OK = len(rec.Missing) == 0 && rec.Error == ""
		if !rec.OK {
			failed = true
		}
		recs = append(recs, rec)
	}

	if asJSON {
		b, _ := json.MarshalIndent(recs, "", "  ")
		fmt.Fprintln(out, string(b))
	} else {
		for _, r := range recs {
			switch {
			case r.Error != "":
				fmt.Fprintf(out, "%-28s ERROR  %s\n", r.Target, r.Error)
			case r.OK && len(r.NoManifest) == 0:
				fmt.Fprintf(out, "%-28s ok      %d/%d artifact(s) present, manifests included\n", r.Target, r.Present, r.Checked)
			case r.OK:
				// The archive is where it should be; the sidecar is not. That is
				// restorable (blindly) and not a lost backup, so it is a warning.
				fmt.Fprintf(out, "%-28s ok      %d/%d present, %d without a manifest (restorable, not verifiable)\n",
					r.Target, r.Present, r.Checked, len(r.NoManifest))
				for _, m := range r.NoManifest {
					fmt.Fprintf(out, "    no manifest: %s\n", m)
				}
			default:
				fmt.Fprintf(out, "%-28s FAILED  %d/%d present, %d missing, %d without a manifest\n",
					r.Target, r.Present, r.Checked, len(r.Missing), len(r.NoManifest))
				for _, m := range r.Missing {
					fmt.Fprintf(out, "    missing: %s\n", m)
				}
				if len(r.Sample) > 0 {
					fmt.Fprintf(out, "    bucket holds (newest): %s\n", strings.Join(r.Sample, ", "))
					fmt.Fprintf(out, "    objects seen in bucket: %d\n", r.Objects)
				}
				for _, m := range r.NoManifest {
					fmt.Fprintf(out, "    no manifest: %s\n", m)
				}
			}
		}
	}

	// Record each target's verdict. `offsite-status` and the doctor read the
	// LAST record, so a failing target is recorded last: writing them in
	// configuration order let a healthy bucket overwrite a broken one and made
	// the summary read as fine.
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].OK && !recs[j].OK })
	for _, r := range recs {
		_ = recordOffsiteCheck(stateDir, r)
	}

	if failed {
		return fmt.Errorf("offsite verification failed — an artifact this node believes it shipped is not in the bucket")
	}
	return nil
}

// recentArtifacts lists archive files under dir newer than days, newest first.
func recentArtifacts(dir string, days int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read archive dir %s: %w", dir, err)
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") || strings.Contains(e.Name(), ".json") {
			continue
		}
		// pmcluster-ctlplane-* are control-plane snapshots shed here for the
		// tarball fallback; they are not per-stack artifacts and carry no
		// manifest. `cluster kit verify` is the drill for those.
		if strings.HasPrefix(e.Name(), "pmcluster-ctlplane-") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

var offsiteStatusCmd = &cobra.Command{
	Use:   "offsite-status",
	Short: "Show the last offsite verification (and two weeks of history)",
	Args:  cobra.NoArgs,
	RunE:  runOffsiteStatus,
}

func runOffsiteStatus(cmd *cobra.Command, _ []string) error {
	stateDir := envOr("PMCLUSTER_BACKUP_STATE", "/var/lib/pmcluster-backup")
	p := offsiteStatusPath(stateDir)
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(cmd.OutOrStdout(), "no offsite check has run on this node yet (%s)\n", p)
			return nil
		}
		return err
	}
	var doc offsiteStatusFile
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", p, err)
	}
	if doc.Last == nil {
		fmt.Fprintln(cmd.OutOrStdout(), "no offsite check recorded")
		return nil
	}
	last := doc.Last
	verdict := "ok"
	if !last.OK {
		verdict = "FAILED"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s — %s (%s): %d/%d present, %d missing, %d without manifest\n",
		last.At.UTC().Format(time.RFC3339), verdict, last.Target,
		last.Present, last.Checked, len(last.Missing), len(last.NoManifest))
	for _, m := range last.Missing {
		fmt.Fprintf(cmd.OutOrStdout(), "  missing: %s\n", m)
	}
	if last.Error != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "  error: %s\n", last.Error)
	}
	if len(doc.History) > 1 {
		fmt.Fprintln(cmd.OutOrStdout(), "\nhistory:")
		for _, h := range doc.History[1:] {
			v := "ok"
			if !h.OK {
				v = "FAILED"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  %s  %-10s %d/%d\n", h.At.UTC().Format("2006-01-02T15:04"), v, h.Present, h.Checked)
		}
	}
	return nil
}
