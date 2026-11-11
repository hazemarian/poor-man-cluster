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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/controlplane"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodever"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
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
	// GitHubApp mints and probes an installation token. Optional; nil means
	// this process has no store/cipher to build one from, and the check skips
	// with that reason.
	GitHubApp GitHubAppProbe
	// GitHubRunners inspects the self-hosted runner lifecycle. Optional; nil
	// means this process has no runner manager to ask, and the check skips
	// with that reason.
	GitHubRunners RunnerProbe
	// Now is the clock the credential-age check compares against. Zero means
	// time.Now(); tests set it to age a credential without waiting 60 days.
	Now time.Time
}

// RunnerProbe is the one capability the github runners check needs. The
// runners.Manager implements it; a test substitutes a stub.
type RunnerProbe interface {
	// Inspect reads the settings, the container labels and the live GitHub
	// registrations without changing anything.
	Inspect(ctx context.Context) (runners.Report, error)
}

// GitHubAppProbe is the one capability the GitHub App check needs. Local
// implements it; a test can substitute a stub without a network.
type GitHubAppProbe interface {
	// Diagnose mints a token and asserts the configured repo read / package
	// HEAD. Errors wrap the githubapp sentinels.
	Diagnose(ctx context.Context) (detail string, expiresAt time.Time, err error)
}

// Run executes every check. An error is returned only when the report itself
// could not be produced; individual checks record their own failure.
func Run(ctx context.Context, in Input) (*Report, error) {
	rep := &Report{At: time.Now().UTC()}
	add := func(c Check) { rep.Checks = append(rep.Checks, c) }

	add(checkSwarm(ctx, in))
	add(checkLeaderLabel(ctx, in))
	add(checkNodeLabels(ctx, in))
	add(checkSkew(ctx, in))
	add(checkKit(ctx, in))
	add(checkSecrets(ctx, in))
	add(checkGitHubApp(ctx, in))
	add(checkGitHubRunners(ctx, in))
	add(checkRegistryCredential(ctx, in))
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

// checkLeaderLabel: does exactly the current leader carry pmcluster.leader=true?
//
// The collector is pinned to that label by a hard placement CONSTRAINT, so the
// label is not cosmetic: when no node carries it the collector cannot schedule at
// all (cluster update refuses to deploy in that state), and when a node other than
// the leader carries it the collector writes the node-stats JSONL onto a disk the
// leader's daemon never reads — an empty panel with no error anywhere.
func checkLeaderLabel(ctx context.Context, in Input) Check {
	if in.Docker == nil {
		return Check{"leader label", StatusSkip, "no docker client"}
	}
	nodes, err := in.Docker.NodeList(ctx)
	if err != nil {
		return Check{"leader label", StatusFail, "node list: " + err.Error()}
	}
	if len(nodes) == 0 {
		return Check{"leader label", StatusSkip, "the node list is empty: no swarm inventory to read the label from"}
	}
	var carriers []string
	leader := ""
	for _, n := range nodes {
		if n.Labels[runtime.LeaderNodeLabel] == "true" && n.Hostname != "" {
			carriers = append(carriers, n.Hostname)
		}
		if n.IsLeader && n.Hostname != "" {
			leader = n.Hostname
		}
	}
	switch {
	case len(carriers) == 0:
		return Check{"leader label", StatusFail,
			cluster.NoLeaderLabelCondition + " (nothing pins the collector, so no task can start)"}
	case len(carriers) > 1:
		return Check{"leader label", StatusWarn,
			fmt.Sprintf("%d nodes carry %s=true (%s) — the collector's constraint matches all of them",
				len(carriers), runtime.LeaderNodeLabel, strings.Join(carriers, ", "))}
	case leader != "" && carriers[0] != leader:
		return Check{"leader label", StatusWarn,
			fmt.Sprintf("%s=true is on %s, but the swarm leader is %s — the collector writes its node-stats file where the leader's daemon does not read it",
				runtime.LeaderNodeLabel, carriers[0], leader)}
	}
	return Check{"leader label", StatusOK, fmt.Sprintf("%s=%s=true", carriers[0], runtime.LeaderNodeLabel)}
}

// checkNodeLabels: does any pmcluster.* node label carry an empty value?
//
// An empty-valued label is not "no label": the key is still there, so anything
// matching on the key's presence reads the node as labelled. That is how a
// worker came to hold `pmcluster.storage: ""` and look like a storage node while
// every value comparison said otherwise. SetNodeLabel removes the key when the
// value is empty; this check reports the labels that were written before that
// (or by hand) so they get cleared instead of silently matching.
func checkNodeLabels(ctx context.Context, in Input) Check {
	if in.Docker == nil {
		return Check{"node labels", StatusSkip, "no docker client"}
	}
	nodes, err := in.Docker.NodeList(ctx)
	if err != nil {
		return Check{"node labels", StatusSkip, "cannot read the node list from this node (a worker cannot): " + err.Error()}
	}
	empty := cluster.EmptyNodeLabels(nodes)
	if len(empty) > 0 {
		return Check{"node labels", StatusWarn,
			fmt.Sprintf("%d pmcluster label(s) carry an empty value: %s — the key is still present, so anything matching on presence reads it as set. "+
				"Clear them: docker node update --label-rm <key> <node>",
				len(empty), strings.Join(empty, ", "))}
	}
	return Check{"node labels", StatusOK, fmt.Sprintf("%d node(s) carry no empty-valued pmcluster label", len(nodes))}
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

// checkGitHubApp: can the cluster mint an installation token, and does that
// installation actually grant what the cluster needs?
//
// It answers four distinct things because they have four distinct fixes:
//
//   - no app configured → skip (a cluster pulling only public images is fine);
//   - the key/id is rejected → the App id and the .pem do not belong together;
//   - the installation is gone → reinstall the app on the org;
//   - a permission is missing → edit the App, then approve the new permissions
//     on the installation. GitHub does not apply App permission edits until the
//     installation approves them, which is why this check reads the granted set
//     instead of trusting the App's settings page.
func checkGitHubApp(ctx context.Context, in Input) Check {
	if in.GitHubApp == nil {
		return Check{"github app", StatusSkip, "no store/cipher available to read the app registration"}
	}
	detail, expires, err := in.GitHubApp.Diagnose(ctx)
	switch {
	case err == nil:
		return Check{"github app", StatusOK, detail}
	case errors.Is(err, githubapp.ErrNoApp):
		return Check{"github app", StatusSkip,
			"no GitHub App configured (public images and a stored registry credential still work); configure one with `pmcluster github app set`"}
	case errors.Is(err, githubapp.ErrNoProbeTarget):
		when := "an installation token was minted"
		if !expires.IsZero() {
			when = "an installation token was minted, valid until " + expires.UTC().Format(time.RFC3339)
		}
		return Check{"github app", StatusWarn,
			when + ", but no probe target is configured — set --probe-repo and --probe-package so read access is actually proven"}
	case errors.Is(err, githubapp.ErrBadKey):
		return Check{"github app", StatusFail, "the app key/id was rejected: " + err.Error()}
	case errors.Is(err, githubapp.ErrInstallationNotFound):
		return Check{"github app", StatusFail, "the installation is gone or not visible: " + err.Error()}
	case errors.Is(err, githubapp.ErrInsufficientPermission):
		return Check{"github app", StatusFail, "insufficient permission: " + err.Error()}
	case errors.Is(err, githubapp.ErrTokenRejected):
		return Check{"github app", StatusFail, "the minted token was rejected: " + err.Error()}
	default:
		return Check{"github app", StatusFail, "could not mint an installation token: " + err.Error()}
	}
}

// pullRegistryHost is the registry whose stored credential authenticates image
// pulls. The App covers clone/build/broker; this row is the pull credential,
// and it is the one that expires (a read-only PAT lives 90 days).
const pullRegistryHost = "ghcr.io"

// registryCredentialWarnAge matches the CLI's deploy warning: warn at 60 days
// so a 90-day read-only PAT has a month of deliberate rotation left.
const registryCredentialWarnAge = 60 * 24 * time.Hour

// checkRegistryCredential: is the credential that authenticates image pulls
// approaching expiry? It warns at 60 days and never fails: an ageing
// credential still pulls, and doctor's job is to say so before a deploy finds
// out. The age is the row's created_at, which `registry add` writes and
// `registry add` again resets on rotation.
func checkRegistryCredential(ctx context.Context, in Input) Check {
	const name = "registry credential"
	if in.Store == nil {
		return Check{name, StatusSkip, "no store available to read the stored registry credential"}
	}
	r, err := in.Store.Registry(ctx, pullRegistryHost)
	if errors.Is(err, store.ErrRegistryNotFound) {
		return Check{name, StatusSkip,
			"no stored " + pullRegistryHost + " credential; private image pulls will fail at deploy (the GitHub App covers clone/build/broker, not pulls) — add a read-only PAT with `pmcluster registry add " + pullRegistryHost + "`"}
	}
	if err != nil {
		return Check{name, StatusFail, "read stored credential: " + err.Error()}
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	age := now.Sub(time.Unix(r.CreatedAt, 0))
	if age < 0 {
		age = 0
	}
	days := int(age / (24 * time.Hour))
	rotate := "`pmcluster registry add " + pullRegistryHost + " --username " + r.Username + " --password-stdin`"

	if age >= registryCredentialWarnAge {
		return Check{name, StatusWarn, fmt.Sprintf(
			"the stored %s credential is %d days old; read-only PATs expire at 90 days — rotate with %s",
			pullRegistryHost, days, rotate)}
	}
	return Check{name, StatusOK, fmt.Sprintf(
		"stored %s credential is %d days old (warns at 60, read-only PAT expiry at 90)",
		pullRegistryHost, days)}
}

// checkGitHubRunners: does the self-hosted runner lifecycle agree with reality?
//
// It answers, in this order, because each answer has a different fix:
//
//   - ENABLED or DISABLED, first and plainly;
//   - enabled with no App / no org / no node → the feature cannot work;
//   - enabled with the App missing administration:write → registration will fail
//     with a bare 403; naming it here is the point of the check;
//   - a selected node whose container and GitHub registration disagree → one of
//     them was removed by hand, and a registered-but-not-running runner is a ghost
//     that never picks up a job;
//   - DISABLED with something left behind → the failure mode of a half-disable:
//     a container or a registration that outlived the switch;
//   - a pmc-named runner GitHub knows that the platform does not manage (the
//     operator's hand-made one) → named, not counted as healthy.
func checkGitHubRunners(ctx context.Context, in Input) Check {
	const name = "github runners"
	if in.GitHubRunners == nil {
		return Check{name, StatusSkip, "no runner manager available to inspect (needs a store)"}
	}
	rep, err := in.GitHubRunners.Inspect(ctx)
	if err != nil {
		return Check{name, StatusFail, "could not inspect runners: " + err.Error()}
	}
	if !rep.Enabled {
		if len(rep.Leftovers) == 0 {
			return Check{name, StatusOK, "disabled (github_runners_enabled=false, nothing is deployed)"}
		}
		return Check{name, StatusWarn,
			"disabled (github_runners_enabled=false) but leftovers remain: " + strings.Join(rep.Leftovers, "; ") +
				" — run a reconcile pass to finish the teardown, then check again"}
	}
	head := "enabled (org " + rep.Org + ", group " + rep.Group + ")"
	if rep.Org == "" || len(rep.Nodes) == 0 {
		return Check{name, StatusFail, head + ": " + strings.Join(rep.Warnings, "; ") +
			" — the reconcile refuses to start a runner it cannot register or place"}
	}
	if !rep.AppConfigured {
		return Check{name, StatusFail, head +
			": no GitHub App is configured — runners register with an App installation token; configure one with `pmcluster github app set` (or turn the feature off)"}
	}
	if rep.MissingPermission != "" {
		return Check{name, StatusFail, head + ": the App is missing " + rep.MissingPermission +
			" — grant Organization → Self-hosted runners: Read & write in the App's settings, then approve the new permission on the installation; registration returns 403 until then"}
	}

	var problems, healthy []string
	for _, n := range rep.Nodes {
		label := n.Node
		if n.Role != "" {
			label += " (" + n.Role + ")"
		}
		switch {
		case n.Error != "":
			problems = append(problems, label+": "+n.Error)
		case n.Refused:
			// The reason names the node's RAM and the cap; nothing else needs to
			// be said, and saying "offline" instead would hide an operator action.
			problems = append(problems, label+": "+n.Reason)
		case n.Failures >= 3:
			problems = append(problems, fmt.Sprintf("%s: failed %d consecutive reconcile pass(es): %s", label, n.Failures, n.LastError))
		case strings.EqualFold(n.Role, "manager") || strings.EqualFold(n.Role, "leader"):
			problems = append(problems, label+": a runner is selected on a manager node — a build there marks the node Down and takes the cluster API with it")
		// ContainerKnown guards both container cases: from the leader a worker's
		// containers cannot be inspected, and claiming "no container" about a
		// node you cannot see is how an operator learns to ignore doctor.
		case n.ContainerKnown && !n.ContainerRunning && n.Registered:
			problems = append(problems, label+": registered but no container is running (the reconcile will recreate it)")
		case n.ContainerKnown && n.ContainerRunning && !n.Registered:
			problems = append(problems, label+": container is running but GitHub does not list "+n.RunnerName)
		case len(n.Offline) > 0:
			problems = append(problems, label+": offline or stale registration(s) "+strings.Join(n.Offline, ", "))
		case n.RawSocket:
			problems = append(problems, label+": the container mounts /var/run/docker.sock directly — the socket proxy (github_runner_socket_proxy=true) is what keeps a workflow off the swarm API")
		default:
			healthy = append(healthy, label)
		}
	}
	// An override for a host nobody selected is kept (the node may come back)
	// but it is not silently inert either: it is usually a half-finished rename.
	for _, host := range rep.StaleMemoryNodes {
		problems = append(problems, "github_runner_node_memory sets a cap for "+host+
			", which is not in github_runner_nodes — the override is kept but caps nothing (a node may be temporarily deselected, so it is not removed)")
	}
	var unmanaged []string
	for _, u := range rep.Unmanaged {
		note := u.Name
		if u.SocketKnown && u.RawSocket {
			note += " (mounts /var/run/docker.sock)"
		} else if !u.SocketKnown {
			note += " (not on this node, mounts unknown)"
		}
		unmanaged = append(unmanaged, note)
	}
	if len(problems) > 0 || len(unmanaged) > 0 {
		var parts []string
		if len(problems) > 0 {
			parts = append(parts, strings.Join(problems, "; "))
			if len(healthy) > 0 {
				parts = append(parts, fmt.Sprintf("(%d node(s) healthy)", len(healthy)))
			}
		}
		if len(unmanaged) > 0 {
			parts = append(parts, "unmanaged runner(s) in the org, not managed by this platform: "+
				strings.Join(unmanaged, ", ")+" — they are not covered by the socket proxy and are never counted as healthy")
		}
		return Check{name, StatusWarn, head + ": " + strings.Join(parts, " | ")}
	}
	access := "socket proxy"
	if len(rep.Nodes) > 0 && rep.Nodes[0].DockerAccess == "raw socket" {
		access = "raw docker socket (debug fallback)"
	}
	detail := fmt.Sprintf("%s: %d runner(s) registered and running on %d selected node(s), docker access via %s",
		head, len(healthy), len(rep.Nodes), access)
	// Name the caps that are NOT the cluster default. A per-node override is the
	// whole point of the setting, so doctor states the number the node actually
	// runs with instead of the default every node would otherwise imply.
	var caps []string
	for _, n := range rep.Nodes {
		if n.Cap != "" && n.Cap != rep.DefaultMemory {
			caps = append(caps, n.Node+"="+n.Cap)
		}
	}
	if len(caps) > 0 {
		detail += "; per-node caps " + strings.Join(caps, ", ") + " (default " + orDash(rep.DefaultMemory) + ")"
	}
	return Check{name, StatusOK, detail}
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
