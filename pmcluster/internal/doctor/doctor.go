// Package doctor answers one question — "is this platform healthy right now?"
// — from checks that already exist but were scattered across six commands and a
// weekly timer's status files.
//
// It is deliberately read-only and cheap enough to run interactively: the only
// expensive step is the control-plane restore drill, which is a few MB of tar
// into a temp dir. Anything it cannot check without writing is reported as a
// skipped check with the reason, never silently omitted.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/controlplane"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodever"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Status of one check.
type Status string

const (
	// StatusOK: measured and healthy.
	StatusOK Status = "ok"
	// StatusWarn: measured, degraded, not fatal.
	StatusWarn Status = "warn"
	// StatusFail: measured and broken.
	StatusFail Status = "fail"
	// StatusSkip: could not be measured here, with the reason in Detail.
	StatusSkip Status = "skip"
)

// Check is one question and its answer.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report is the whole answer.
type Report struct {
	At     time.Time `json:"at"`
	Checks []Check   `json:"checks"`
	OK     bool      `json:"ok"`
}

// Input is everything the checks read.
type Input struct {
	Docker runtime.Client
	Store  *store.Store
	Cipher *credentials.Cipher
	// DataDir is the daemon's data dir (kit configs are read from the swarm,
	// the drill extracts to a temp dir).
	DataDir string
	// Version is the running binary version, the reference for skew.
	Version string
	// BackupStateDir holds the drill and offsite status files.
	BackupStateDir string
	// VolumeRoot is where stack data lives, for the free-space check.
	VolumeRoot string
}

// Run executes every check. An error is returned only when the report itself
// could not be produced; individual checks record their own failure.
func Run(ctx context.Context, in Input) (*Report, error) {
	rep := &Report{At: time.Now().UTC()}
	add := func(c Check) { rep.Checks = append(rep.Checks, c) }

	add(checkSwarm(ctx, in))
	add(checkSkew(ctx, in))
	add(checkKit(ctx, in))
	add(checkSecrets(ctx, in))
	add(checkLastApply(ctx, in))
	add(checkDrill(in))
	add(checkOffsite(in))
	add(checkDisk(in))

	rep.OK = true
	for _, c := range rep.Checks {
		if c.Status == StatusFail {
			rep.OK = false
		}
	}
	return rep, nil
}

// checkSwarm: is there a swarm, and can it survive one more loss?
func checkSwarm(ctx context.Context, in Input) Check {
	if in.Docker == nil {
		return Check{"swarm", StatusSkip, "no docker client"}
	}
	info, err := in.Docker.Info(ctx)
	if err != nil {
		return Check{"swarm", StatusFail, err.Error()}
	}
	if !info.SwarmControlAvailable {
		return Check{"swarm", StatusSkip, "this node is a worker: it cannot read the manager set"}
	}
	nodes, err := in.Docker.NodeList(ctx)
	if err != nil {
		return Check{"swarm", StatusFail, "node list: " + err.Error()}
	}
	q := cluster.QuorumOf(nodes)
	if why := cluster.MembershipChangeRefusal(q, "any further membership change"); why != "" {
		return Check{"swarm", StatusFail, why}
	}
	if w := cluster.ManagerCountWarning(q); w != "" {
		return Check{"swarm", StatusWarn, w}
	}
	return Check{"swarm", StatusOK, fmt.Sprintf("%d/%d manager(s) ready, quorum %d", q.Ready, q.Managers, q.Required())}
}

// checkSkew: does every reporting manager run this binary?
func checkSkew(ctx context.Context, in Input) Check {
	if in.Docker == nil {
		return Check{"version skew", StatusSkip, "no docker client"}
	}
	skew, err := nodever.Read(ctx, in.Docker, in.Version)
	if err != nil {
		return Check{"version skew", StatusSkip, err.Error()}
	}
	if len(skew.Nodes) == 0 {
		return Check{"version skew", StatusWarn, "no manager has published a version yet"}
	}
	if !skew.OK() {
		var parts []string
		for _, e := range skew.Stale {
			parts = append(parts, e.Hostname+"="+e.Version)
		}
		return Check{"version skew", StatusWarn,
			fmt.Sprintf("%d of %d manager(s) not on %s: %s", len(skew.Stale), len(skew.Nodes), in.Version, strings.Join(parts, ", "))}
	}
	return Check{"version skew", StatusOK, fmt.Sprintf("%d manager(s) on %s", len(skew.Nodes), in.Version)}
}

// checkKit: does the newest control-plane snapshot restore, and how old is it?
func checkKit(ctx context.Context, in Input) Check {
	if in.Docker == nil {
		return Check{"control-plane kit", StatusSkip, "no docker client"}
	}
	kit := &controlplane.Kit{Docker: in.Docker, DataDir: in.DataDir}
	rep, err := kit.Verify(ctx)
	if err != nil {
		return Check{"control-plane kit", StatusFail, err.Error()}
	}
	detail := fmt.Sprintf("snapshot %s old, %d table(s), %d secret(s) decrypt",
		rep.SnapshotAge, rep.Tables, rep.SecretsOK)
	if len(rep.Warnings) > 0 {
		return Check{"control-plane kit", StatusFail, detail + "; " + strings.Join(rep.Warnings, "; ")}
	}
	if age, perr := time.ParseDuration(rep.SnapshotAge); perr == nil && age > 30*time.Minute {
		return Check{"control-plane kit", StatusWarn, detail + " (snapshot is old for a 5-minute cadence — is this the leader?)"}
	}
	return Check{"control-plane kit", StatusOK, detail}
}

// checkSecrets: does every stored secret decrypt with the current key?
func checkSecrets(ctx context.Context, in Input) Check {
	if in.Store == nil || in.Cipher == nil {
		return Check{"secrets", StatusSkip, "no store/cipher available"}
	}
	rows, err := in.Store.ListSecrets(ctx, "", "")
	if err != nil {
		return Check{"secrets", StatusFail, "list: " + err.Error()}
	}
	var broken []string
	for _, meta := range rows {
		row, rerr := in.Store.Secret(ctx, meta.Name)
		if rerr != nil {
			broken = append(broken, meta.Name+" (read: "+rerr.Error()+")")
			continue
		}
		if _, derr := in.Cipher.Decrypt(row.Payload); derr != nil {
			broken = append(broken, meta.Name)
		}
	}
	if len(broken) > 0 {
		return Check{"secrets", StatusFail,
			fmt.Sprintf("%d of %d secret(s) do not decrypt: %s — re-supply with `pmcluster secret edit`", len(broken), len(rows), strings.Join(broken, ", "))}
	}
	return Check{"secrets", StatusOK, fmt.Sprintf("%d secret(s) decrypt", len(rows))}
}

// checkLastApply: did the last platform apply finish everything it tried?
func checkLastApply(ctx context.Context, in Input) Check {
	if in.Store == nil {
		return Check{"last apply", StatusSkip, "no store available"}
	}
	rec := cluster.LoadApplyRecord(ctx, in.Store)
	if rec == nil {
		return Check{"last apply", StatusSkip, "no apply recorded yet"}
	}
	if len(rec.Warnings) > 0 {
		return Check{"last apply", StatusWarn,
			fmt.Sprintf("%d warning(s) at %s: %s", len(rec.Warnings), rec.At.UTC().Format(time.RFC3339), rec.Warnings[0])}
	}
	return Check{"last apply", StatusOK, rec.Line()}
}

// checkDrill: when was a backup artifact last actually restored?
func checkDrill(in Input) Check {
	if in.BackupStateDir == "" {
		return Check{"restore drill", StatusSkip, "no backup state dir on this host"}
	}
	path := filepath.Join(in.BackupStateDir, "drill", "status.json")
	var doc struct {
		Last *struct {
			At      time.Time `json:"at"`
			OK      bool      `json:"ok"`
			Archive string    `json:"archive"`
		} `json:"last"`
	}
	if err := readJSON(path, &doc); err != nil {
		if os.IsNotExist(err) {
			return Check{"restore drill", StatusWarn, "no drill has run on this node (" + path + ")"}
		}
		return Check{"restore drill", StatusWarn, err.Error()}
	}
	if doc.Last == nil {
		return Check{"restore drill", StatusWarn, "no drill recorded"}
	}
	age := time.Since(doc.Last.At).Truncate(time.Hour)
	if !doc.Last.OK {
		return Check{"restore drill", StatusFail, "last drill FAILED at " + doc.Last.At.UTC().Format(time.RFC3339)}
	}
	if age > 14*24*time.Hour {
		return Check{"restore drill", StatusWarn, fmt.Sprintf("last successful drill %s ago", age)}
	}
	return Check{"restore drill", StatusOK, fmt.Sprintf("last drill %s ago (%s)", age, doc.Last.Archive)}
}

// checkOffsite: are this node's shipped artifacts still in the bucket?
func checkOffsite(in Input) Check {
	if in.BackupStateDir == "" {
		return Check{"offsite", StatusSkip, "no backup state dir on this host"}
	}
	path := filepath.Join(in.BackupStateDir, "offsite", "status.json")
	var doc struct {
		Last *struct {
			At      time.Time `json:"at"`
			OK      bool      `json:"ok"`
			Target  string    `json:"target"`
			Checked int       `json:"checked"`
			Present int       `json:"present"`
			Missing []string  `json:"missing"`
			Error   string    `json:"error"`
		} `json:"last"`
	}
	if err := readJSON(path, &doc); err != nil {
		if os.IsNotExist(err) {
			return Check{"offsite", StatusWarn, "no offsite check has run on this node"}
		}
		return Check{"offsite", StatusWarn, err.Error()}
	}
	if doc.Last == nil {
		return Check{"offsite", StatusWarn, "no offsite check recorded"}
	}
	detail := fmt.Sprintf("%s %s: %d/%d present", doc.Last.At.UTC().Format("2006-01-02T15:04"), doc.Last.Target, doc.Last.Present, doc.Last.Checked)
	switch {
	case doc.Last.Error != "":
		return Check{"offsite", StatusFail, detail + " — " + doc.Last.Error}
	case !doc.Last.OK:
		return Check{"offsite", StatusFail, fmt.Sprintf("%s — %d missing (e.g. %s)", detail, len(doc.Last.Missing), first(doc.Last.Missing))}
	case time.Since(doc.Last.At) > 48*time.Hour:
		return Check{"offsite", StatusWarn, detail + " (stale check)"}
	}
	return Check{"offsite", StatusOK, detail}
}

// checkDisk: is there room for the next artifact and the next snapshot?
func checkDisk(in Input) Check {
	if in.VolumeRoot == "" {
		return Check{"disk", StatusSkip, "volume root unknown"}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(in.VolumeRoot, &st); err != nil {
		return Check{"disk", StatusSkip, err.Error()}
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	free := float64(st.Bavail) * float64(st.Bsize)
	if total == 0 {
		return Check{"disk", StatusSkip, "filesystem reports zero size"}
	}
	pct := 100 * free / total
	const gib = 1024 * 1024 * 1024
	detail := fmt.Sprintf("%.1f%% free (%.1f GiB of %.1f GiB) on %s", pct, free/gib, total/gib, in.VolumeRoot)
	switch {
	case pct < 5:
		return Check{"disk", StatusFail, detail}
	case pct < 15:
		return Check{"disk", StatusWarn, detail}
	}
	return Check{"disk", StatusOK, detail}
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
