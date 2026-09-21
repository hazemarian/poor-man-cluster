package controllers

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// ---------------------------------------------------------------------------
// Backups page plumbing.
//
// The v2 page separates three things the old page ran together:
//   - what a run means      (Verified / Incomplete / Missed, from the recorded
//     status + whether an archive path was written)
//   - whether the stack is safe NOW (coverage of each stack's CURRENT revision)
//   - how stale the last success is
//
// The API exposes no size, no schedule and no restore/prune endpoint, so the
// page says exactly that instead of filling those cells with a plausible zero.
// ---------------------------------------------------------------------------

const (
	// backupsListLimit is the run window the console reads (also the retention
	// figure shown on the page: the API reports no retention policy of its own).
	backupsListLimit = 50
	// coverageStackCap bounds the per-stack coverage fan-out on large clusters.
	coverageStackCap = 40
	// backupStaleDays is how old the last verified backup may be before the
	// console flags a stack as stale.
	backupStaleDays = 7
)

// Backups lists backups and triggers new ones.
type Backups struct{ *Controller }

type backupsData struct {
	Runs      []backupRunRow
	RunsKnown bool
	RunCount  int64
	Limit     int64

	Coverage        []backupCoverageRow
	CoverageKnown   bool
	CoveragePartial bool
	Checked         int64
	StackCount      int64
	Unchecked       int64

	LastVerifiedDays int64
	HasLastVerified  bool
	AtRisk           int64
	Stale            int64
	WarnDays         int64

	ErrKey string
	ErrRaw string
	MsgKey string
	MsgA   string

	// --- compatibility surface, read by the fragment as a fallback ----------
	// stacks.go's per-stack route (ShowBackups) renders this same fragment
	// scoped to one stack and still fills the pre-redesign fields. .Backups
	// holds that stack's runs and .Error its raw failure, so that route keeps
	// rendering until its owner switches to .Runs/.ErrKey.
	Name    string
	Error   string
	Backups []backupRunRow
}

type backupRunRow struct {
	ID     int64
	Status string // the daemon's own word, always shown as-is
	// StateKey is the shared status word (st.*), empty when the daemon reports a
	// state this console does not know — the row then shows the raw word.
	StateKey        string
	Pill            string // good | warn | bad | info | plain
	Stack           string
	Cluster         bool // a cluster-wide run (no stack name)
	Revision        int64
	Started         int64
	Finished        int64
	DurationSeconds int64
	Running         bool
	Archives        int64
	ArchivePaths    string // newline-joined, for the copy button
	RunError        string
}

type backupCoverageRow struct {
	Stack        string
	Current      int64
	LastRevision int64
	HasLast      bool
	StateKey     string // backups.coverage_* dictionary key
	Pill         string
	Verified     bool
	LastAt       int64
	HasLastAt    bool
	AgeDays      int64
	Stale        bool
	Unreadable   bool
}

// backupState maps a recorded run onto a state word. A run only counts as
// verified when the daemon says it succeeded AND an archive path was recorded —
// exactly the definition the page spells out in its legend.
// rowsCoverCluster reports whether the newest verified run (at verifiedAt) is
// a whole-disk cluster-wide snapshot — a run with no stack name that captures
// every stack's current data on disk, so it covers whatever revision is live.
func rowsCoverCluster(rows []pmapi.Backup, verifiedAt int64) bool {
	for _, r := range rows {
		if r.StartedAt != verifiedAt {
			continue
		}
		key, _, _ := backupState(r)
		return key == "st.verified" && r.StackName == ""
	}
	return false
}

func backupState(b pmapi.Backup) (key, pill string, running bool) {
	status := strings.ToLower(strings.TrimSpace(b.Status))
	switch {
	case b.FinishedAt == 0 && (status == "" || status == "running" || status == "pending" || status == "started" || status == "in_progress"):
		return "st.running", "info", true
	case status == "succeeded" || status == "success" || status == "ok" || status == "done" || status == "completed":
		if len(b.ArchivePaths) > 0 {
			return "st.verified", "good", false
		}
		// Finished without an archive: nothing can be restored from it.
		return "st.incomplete", "warn", false
	case status == "failed" || status == "error":
		return "st.failed", "bad", false
	case status == "partial" || status == "incomplete":
		return "st.incomplete", "warn", false
	}
	return "", "plain", false
}

func backupRunRowFrom(b pmapi.Backup) backupRunRow {
	row := backupRunRow{
		ID: b.ID, Status: b.Status, Stack: b.StackName, Revision: b.Revision,
		Started: b.StartedAt, Finished: b.FinishedAt,
		Archives:     int64(len(b.ArchivePaths)),
		ArchivePaths: strings.Join(b.ArchivePaths, "\n"),
		RunError:     b.ErrorMessage,
	}
	row.Cluster = b.StackName == ""
	row.StateKey, row.Pill, row.Running = backupState(b)
	if row.Running {
		row.Archives = int64(len(b.ArchivePaths))
	} else if b.FinishedAt > b.StartedAt && b.StartedAt > 0 {
		row.DurationSeconds = b.FinishedAt - b.StartedAt
	}
	return row
}

// backupRestoreRoot is where a restore writes: the volume root the stack file
// mounts on the daemon's node. The daemon takes an explicit dest_root, and the
// console states it rather than sending an empty string, so the page can show
// the operator where the files went.
const backupRestoreRoot = "/var/stack/data"

// backupBrowseData is the view model of frag_backupbrowse.html: one run's
// archive listing, opened from the backups table.
type backupBrowseData struct {
	ID    int64
	Run   *pmapi.Backup
	Files []pmapi.BackupFile
	Known bool
	Count int64

	// StateKey/Pill are the state word of the run, from the same mapping the
	// runs table uses, so the two pages cannot disagree about a run.
	StateKey string
	Pill     string

	// Restorable says whether the restore action is offered: a finished run that
	// belongs to a stack. A cluster-scope run has no stack volumes to write into.
	Restorable bool

	// DestRoot is where a restore would write. It is read from the daemon's own
	// volume_root setting when it reports one, so the confirmation names the
	// directory the daemon will actually use rather than a guess.
	DestRoot string

	ErrKey   string
	ErrRaw   string
	MsgKey   string
	MsgCount int64
	MsgPath  string
}

// Browse renders the archive listing of one run.
func (c Backups) Browse(g *gin.Context) {
	ctx := g.Request.Context()
	d := backupBrowseData{}
	if !c.browseParams(g, &d) {
		c.Views.Fragment(g, "backupbrowse", d)
		return
	}
	c.loadBrowse(ctx, &d)
	c.Views.Fragment(g, "backupbrowse", d)
}

// Restore extracts a run's archives back under backupRestoreRoot and re-renders
// the listing, so the operator sees the state the daemon is in now.
func (c Backups) Restore(g *gin.Context) {
	ctx := g.Request.Context()
	d := backupBrowseData{}
	if !c.browseParams(g, &d) {
		c.Views.Fragment(g, "backupbrowse", d)
		return
	}
	n, err := c.API.RestoreBackup(ctx, d.ID, d.DestRoot)
	if err != nil {
		d.ErrKey, d.ErrRaw = "err.backup_restore", err.Error()
	} else {
		d.MsgKey, d.MsgCount, d.MsgPath = "backupbrowse.restored", int64(n), backupRestoreRoot
	}
	c.loadBrowse(ctx, &d)
	c.Views.Fragment(g, "backupbrowse", d)
}

// browseParams records the two states that are not listing failures — no daemon
// configured, and an id that is not a number — and resolves the run id.
func (c Backups) browseParams(g *gin.Context, d *backupBrowseData) bool {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	if !configured {
		d.ErrKey = "err.api_not_configured"
		return false
	}
	id, err := strconv.ParseInt(strings.TrimSpace(g.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		d.ErrKey = "backupbrowse.err_id"
		return false
	}
	d.ID = id
	d.DestRoot = backupRestoreRoot
	// Best effort: when the daemon reports its volume root, that is where a
	// restore lands. A failed read is not an error here — the default stands and
	// the page shows it, so the operator is never told a path that was not read.
	if cs, err := c.API.GetClusterSettings(ctx); err == nil {
		if vr := strings.TrimSpace(cs["volume_root"]); vr != "" {
			d.DestRoot = vr
		}
	}
	return true
}

// loadBrowse reads the run and its files. A failure never overwrites an error
// the caller already recorded: a restore failure is the more specific answer.
func (c Backups) loadBrowse(ctx context.Context, d *backupBrowseData) {
	run, files, err := c.API.BrowseBackup(ctx, d.ID)
	if err != nil {
		if d.ErrKey == "" {
			d.ErrKey, d.ErrRaw = "err.backup_browse", err.Error()
		}
		return
	}
	d.Run, d.Files, d.Known, d.Count = run, files, true, int64(len(files))
	if run != nil {
		d.StateKey, d.Pill, _ = backupState(*run)
		// Whole-disk (cluster-wide) runs are restorable too — the restore
		// path maps their archives back into the volume root directly.
		d.Restorable = run.FinishedAt > 0 &&
			(d.StateKey == "st.verified" || d.StateKey == "st.incomplete")
	}
}

// daysAgo converts a unix timestamp into whole days elapsed (never negative).
func daysAgo(ts int64) int64 {
	if ts <= 0 {
		return 0
	}
	d := (time.Now().Unix() - ts) / 86400
	if d < 0 {
		return 0
	}
	return d
}

// newest returns the index of the most recently started run, or -1.
func newest(rows []pmapi.Backup) int {
	idx, at := -1, int64(-1)
	for i, r := range rows {
		if r.StartedAt > at {
			idx, at = i, r.StartedAt
		}
	}
	return idx
}

// List renders the coverage table and the recent run history.
func (c Backups) List(g *gin.Context) {
	_, _, configured := c.loadParams(g.Request.Context())
	d := backupsData{Limit: backupsListLimit, WarnDays: backupStaleDays}
	if !configured {
		d.ErrKey = errKeyNotConfigured
		c.Views.Fragment(g, "backups", d)
		return
	}
	c.load(g, &d)
	c.Views.Fragment(g, "backups", d)
}

// load reads the run window and the per-stack coverage into d.
func (c Backups) load(g *gin.Context, d *backupsData) {
	ctx := g.Request.Context()

	if rows, err := c.API.ListBackups(ctx, backupsListLimit); err != nil {
		if d.ErrKey == "" {
			d.ErrKey, d.ErrRaw = humanErr("err.backups", err)
		}
	} else {
		d.RunsKnown = true
		for _, b := range rows {
			d.Runs = append(d.Runs, backupRunRowFrom(b))
		}
		d.RunCount = int64(len(d.Runs))
		for _, r := range d.Runs {
			if r.StateKey == "st.verified" {
				d.HasLastVerified = true
				age := daysAgo(r.Started)
				if !d.HasLastVerified || age < d.LastVerifiedDays || d.LastVerifiedDays == 0 {
					d.LastVerifiedDays = age
				}
			}
		}
	}

	c.coverage(g, d)
}

// coverage checks whether every stack's CURRENT revision has a verified backup.
// A stack whose own history could not be read is reported as unreadable and
// flips the partial flag, so the list is never presented as complete.
func (c Backups) coverage(g *gin.Context, d *backupsData) {
	ctx := g.Request.Context()
	stacks, err := c.API.ListStacks(ctx)
	if err != nil {
		if d.ErrKey == "" {
			d.ErrKey, d.ErrRaw = humanErr("err.backups_coverage", err)
		}
		return
	}
	d.CoverageKnown = true
	d.StackCount = int64(len(stacks))
	if len(stacks) > coverageStackCap {
		d.CoveragePartial = true
		d.Unchecked = int64(len(stacks) - coverageStackCap)
		stacks = stacks[:coverageStackCap]
	}
	d.Checked = int64(len(stacks))

	for _, st := range stacks {
		row := backupCoverageRow{Stack: st.Name, Current: st.CurrentRevision}
		rows, err := c.API.ListStackBackups(ctx, st.Name)
		if err != nil {
			row.Unreadable = true
			row.StateKey, row.Pill = "backups.coverage_unknown", "warn"
			d.CoveragePartial = true
			d.Coverage = append(d.Coverage, row)
			continue
		}

		if idx := newest(rows); idx >= 0 {
			last := rows[idx]
			row.LastAt, row.HasLastAt = last.StartedAt, last.StartedAt > 0
			row.AgeDays = daysAgo(last.StartedAt)
			row.Stale = row.AgeDays > backupStaleDays
		}

		// The newest run that actually recorded archives decides how far behind
		// the stack is; a finished-but-empty or failed run does not count.
		verifiedAt, verifiedRev, hasVerified := int64(-1), int64(0), false
		anyVerified, anyFailed := false, false
		for _, r := range rows {
			key, _, _ := backupState(r)
			switch key {
			case "st.verified":
				anyVerified = true
				if r.StartedAt > verifiedAt {
					verifiedAt, verifiedRev, hasVerified = r.StartedAt, r.Revision, true
				}
			case "st.failed":
				anyFailed = true
			}
		}
		if hasVerified {
			row.HasLast, row.LastRevision = true, verifiedRev
		}

		// A whole-disk (cluster-wide) run has no stack name and no revision —
		// it snapshots every stack's current data on disk, so it verifies
		// whatever revision is live right now.
		clusterWide := hasVerified && rowsCoverCluster(rows, verifiedAt)

		switch {
		case hasVerified && (clusterWide || verifiedRev == st.CurrentRevision):
			row.StateKey, row.Pill, row.Verified = "backups.coverage_verified", "good", true
		case hasVerified:
			row.StateKey, row.Pill = "backups.coverage_behind", "warn"
		case anyFailed:
			row.StateKey, row.Pill = "backups.coverage_failed", "bad"
		case !anyVerified:
			row.StateKey, row.Pill = "backups.coverage_none", "bad"
			if len(rows) == 0 {
				row.HasLastAt, row.Stale = false, false
			}
		}
		if !row.Verified {
			d.AtRisk++
		}
		if row.Stale {
			d.Stale++
		}
		d.Coverage = append(d.Coverage, row)
	}
}

// Create triggers a backup and re-renders the page.
func (c Backups) Create(g *gin.Context) {
	d := backupsData{Limit: backupsListLimit, WarnDays: backupStaleDays}
	if _, _, configured := c.loadParams(g.Request.Context()); !configured {
		d.ErrKey = errKeyNotConfigured
		c.Views.Fragment(g, "backups", d)
		return
	}
	if _, err := c.API.CreateBackup(g.Request.Context()); err != nil {
		d.ErrKey, d.ErrRaw = humanErr("err.backup_create", err)
	} else {
		d.MsgKey = "backups.msg_triggered"
	}
	c.load(g, &d)
	c.Views.Fragment(g, "backups", d)
}
