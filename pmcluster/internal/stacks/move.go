package stacks

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Move relocates a stateful stack's storage to another swarm node:
//
//  1. validates the stack has stateful (volume-holding) services,
//  2. triggers a whole-disk on-demand backup (unanchored row),
//  3. restores the stack's <VolumeRoot>/<stack> subtree on the target — locally
//     when the target is this host (or Docker is unavailable), otherwise via a
//     one-shot swarm mover service that pulls the archive from this host,
//  4. pins the stack to the target via the stack_pin_<stack> setting,
//  5. re-deploys so the pin is rendered into the placement constraint.
//
// The reconcile loop never moves the stack back: the per-stack pin outranks
// the storage_nodes round-robin and the platform_node fallback in
// PinResolver.ResolvePlacement. The source node's old <VolumeRoot>/<stack>
// subtree is intentionally left in place as a safety net until the operator
// prunes it.
func (s *Service) Move(ctx context.Context, stackName, targetNode string) error {
	return s.MoveWithOptions(ctx, stackName, targetNode, MoveOptions{})
}

// Ack resolves an open storage-failover marker without moving the stack: the
// operator accepts that it now runs on the failover target. After ack the
// console banner and the GitHub badge return to the stack's normal health.
func (s *Service) Ack(ctx context.Context, stackName string) error {
	return s.Store.AckStackFailover(ctx, stackName)
}

// MoveOptions tunes how a stack move sources its data.
type MoveOptions struct {
	// FromS3 fetches the newest known archive for the stack from the offsite
	// object store instead of triggering a local backup. Used by the storage
	// failover path: when the source storage node is down, its local archive
	// is unreachable, so the last S3 upload is the only copy of the data.
	FromS3 bool
}

// MoveWithOptions is Move with restore-source control (see Move for the
// default local-backup flow).
func (s *Service) MoveWithOptions(ctx context.Context, stackName, targetNode string, opts MoveOptions) error {
	if targetNode == "" {
		return fmt.Errorf("move: target node is empty (use --to <node>)")
	}
	revs, err := s.Store.ListRevisions(ctx, stackName, 1)
	if err != nil {
		return fmt.Errorf("move: list revisions: %w", err)
	}
	if len(revs) == 0 {
		return fmt.Errorf("move: stack %q not found", stackName)
	}
	latest := revs[0]

	pins, err := s.StoragePinsForStack(ctx, stackName)
	if err != nil {
		return fmt.Errorf("move: resolve storage pins: %w", err)
	}
	if len(pins) == 0 {
		return fmt.Errorf("move: stack %q has no stateful storage to move (no volume-holding services)", stackName)
	}
	if len(pins) == 1 && pins[0] == targetNode {
		return fmt.Errorf("move: stack %q storage is already on %s", stackName, targetNode)
	}

	// Node validation + leader discovery when the swarm is reachable.
	localHost, _ := os.Hostname()
	var leaderAddr string
	// nodes is declared here (not inside the if) so the store-transit branch
	// below can map the stack's pinned source hostname to its swarm node id.
	var nodes []runtime.Node
	if s.Docker != nil {
		nodes, err = s.Docker.NodeList(ctx)
		if err != nil {
			return fmt.Errorf("move: list swarm nodes: %w", err)
		}
		targetOK := false
		for _, n := range nodes {
			if n.Hostname == targetNode {
				if n.Status == "ready" && n.Availability == "active" {
					targetOK = true
				} else {
					return fmt.Errorf("move: target node %s is not ready/active (status=%s availability=%s)", targetNode, n.Status, n.Availability)
				}
			}
			if n.IsLeader {
				leaderAddr = n.Address
			}
		}
		if !targetOK {
			return fmt.Errorf("move: target node %s not found in swarm", targetNode)
		}
	}

	volRoot := s.VolumeRoot
	if volRoot == "" {
		volRoot = manifest.DefaultVolumeRoot
	}
	crossNode := s.Docker != nil && targetNode != localHost

	// 1+2. Get the stack's data onto the target.
	//
	// Store-based transit: a mover task on the target pulls the archive
	// OBJECT straight out of the object store over the published loopback
	// ingress (127.0.0.1:8333 on every node), so a move needs no cross-node
	// host ports and therefore no firewall rules. The fallback (a cluster
	// without a store configured, or Docker unavailable) keeps the old
	// local-archive + ephemeral HTTP mover path.
	if crossNode && s.S3.Configured() {
		if !opts.FromS3 {
			// Manual move: take a fresh backup first so the object we pull is
			// current. The failover path must NOT do this — the data lives on
			// the node that just failed, not on this host.
			if _, err := s.runMoveBackup(ctx); err != nil {
				return err
			}
		}
		// The failover restore must pull the SOURCE node's archive, not the
		// globally-newest one (that is often the target's own archive, which
		// holds zero backup/data/<stack> entries and would restore an empty
		// database). pins[0] is the node the stack's data currently lives on
		// (the failed/pinned node); map it to its swarm node id. When Docker
		// is unavailable (a CLI run without a docker client) there is no node
		// list to map through, so sourceNode stays empty and the newest-
		// overall behaviour is preserved.
		sourceNode := ""
		for _, n := range nodes {
			if n.Hostname == pins[0] {
				sourceNode = n.ID
				break
			}
		}
		if s.Docker != nil && sourceNode == "" {
			return fmt.Errorf("move: cannot resolve the node id of %s (the node holding the stack's data)", pins[0])
		}
		key, err := s.newestArchiveObject(ctx, stackName, sourceNode)
		if err != nil {
			return err
		}
		if err := s.moveViaStorePull(ctx, key, stackName, targetNode); err != nil {
			return fmt.Errorf("move: store transit: %w", err)
		}
	} else {
		var archivePath string
		if opts.FromS3 {
			archivePath, err = s.fetchNewestArchiveFromS3(ctx, stackName)
			if err != nil {
				return err
			}
		} else {
			// Whole-disk backup (unanchored row, so the restore's volume filter
			// can match the <stack>/ prefix via the bare-volume branch of
			// volumeMatch).
			archivePath, err = s.runMoveBackup(ctx)
			if err != nil {
				return err
			}
		}
		if crossNode {
			if err := s.moveViaMover(ctx, archivePath, volRoot, stackName, targetNode, leaderAddr); err != nil {
				return fmt.Errorf("move: mover transit: %w", err)
			}
		} else {
			if _, err := extractStackSubtree(archivePath, volRoot, stackName); err != nil {
				return fmt.Errorf("move: restore %s subtree: %w", stackName, err)
			}
		}
	}

	// 3. Pin the stack to the target. stack_pin_<stack> is deliberately NOT in
	// the settings allowlist, so the settings API/CLI cannot edit it — Move
	// writes it directly and only Move removes it (via a move to a new node).
	if err := s.Store.SetSetting(ctx, StackPinKey(stackName), targetNode); err != nil {
		return fmt.Errorf("move: pin %s to %s: %w", stackName, targetNode, err)
	}

	// 4. Re-deploy so the pin renders into the placement constraint and the
	// reconcile loop sees a fresh revision. The rendered hash changes (the
	// resolved hostname changed), so drift detection keeps it applied.
	if _, err := s.reconcileDeploy(ctx, stackName, latest.SourceYAML); err != nil {
		return fmt.Errorf("move: redeploy %s: %w", stackName, err)
	}

	// 5. A successful move is an operator action that resolves any open
	// failover: the stack now lives where it was deliberately put, so clear the
	// unacknowledged marker (badge/console return to normal health). Absent
	// marker is not an error.
	if err := s.Store.ClearStackFailover(ctx, stackName); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.Log.Warn().Err(err).Str("stack", stackName).Msg("move: could not clear failover marker")
	}
	return nil
}

// fetchNewestArchiveFromS3 downloads the newest succeeded archive known to the
// store from the offsite object store into BackupDir, returning the local
// path. Used by the storage-failover move path when the source node is down.
func (s *Service) fetchNewestArchiveFromS3(ctx context.Context, stackName string) (string, error) {
	if !s.S3.Configured() {
		return "", fmt.Errorf("move: S3 restore requested but backup_s3_* settings are not configured")
	}
	// The newest succeeded backup row names the archive: the DB stores the
	// local container-style path; its basename is the S3 object key (offen
	// uploads under the same BACKUP_FILENAME it writes locally).
	var newest *store.Backup
	rows, err := s.Store.ListBackups(ctx, 100)
	if err != nil {
		return "", fmt.Errorf("move: list backups for S3 restore: %w", err)
	}
	for _, b := range rows {
		if b.Status != "succeeded" || b.ArchivePaths == "" {
			continue
		}
		if newest == nil || b.StartedAt > newest.StartedAt {
			newest = b
		}
	}
	if newest == nil {
		return "", fmt.Errorf("move: no succeeded backup row found to restore %s from S3 (run `pmcluster backup create` first)", stackName)
	}
	key := filepath.Base(strings.Split(newest.ArchivePaths, ",")[0])
	if !strings.HasSuffix(key, ".tar.gz") {
		return "", fmt.Errorf("move: newest succeeded archive %q does not look like a backup archive", key)
	}
	if err := os.MkdirAll(s.moveBackupDir(), 0o755); err != nil {
		return "", fmt.Errorf("move: create backup dir: %w", err)
	}
	dst := filepath.Join(s.moveBackupDir(), "s3restore-"+key)
	if err := backups.FetchS3Object(ctx, s.S3, key, dst); err != nil {
		return "", fmt.Errorf("move: fetch %s from S3: %w", key, err)
	}
	return dst, nil
}

// runMoveBackup triggers a whole-disk backup through the same machinery as a
// pre-deploy backup, but records an UNANCHORED row (empty stack name / zero
// revision) so the restore step's volume filter matches by bare prefix. The
// archive on disk is located by scanning BackupDir for the newest backup-*.tar.gz
// created during this run (the trigger's stdout may carry container-style
// /archive/... paths that are meaningless on the host).
func (s *Service) runMoveBackup(ctx context.Context) (string, error) {
	id, err := s.Store.CreateBackup(ctx, "", 0)
	if err != nil {
		return "", fmt.Errorf("move: create backup record: %w", err)
	}
	if s.Backup == nil {
		_ = s.Store.FinishBackup(ctx, id, "failed", "", "no BackupTrigger configured (stacks.Service.Backup is nil)")
		return "", fmt.Errorf("move: no backup trigger configured")
	}
	started := time.Now().Unix()
	paths, err := s.Backup.Trigger(ctx)
	if err != nil {
		_ = s.Store.FinishBackup(ctx, id, "failed", strings.Join(paths, ","), err.Error())
		return "", fmt.Errorf("move: backup trigger: %w", err)
	}
	_ = s.Store.FinishBackup(ctx, id, "succeeded", strings.Join(paths, ","), "")

	// The trigger reports container-style /archive/... paths (the offen
	// agent mounts BackupDir at /archive); map them back to host paths via
	// the basename. Fall back to the newest backup-*.tar.gz created during
	// this run when stdout carried no usable token.
	if p := s.resolveArchivePath(ctx, started, paths); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("move: backup ran but produced no archive under %s (trigger stdout: %s)", s.moveBackupDir(), strings.Join(paths, ","))
}

// resolveArchivePath picks the archive to restore from: the newest host path
// that exists among the given container-style trigger paths, else the newest
// backup-*.tar.gz in the archive root created at/after at.
func (s *Service) resolveArchivePath(ctx context.Context, at int64, triggerPaths []string) string {
	backupDir := s.moveBackupDir()
	var newestHost string
	var newestMtime int64
	for _, p := range triggerPaths {
		base := filepath.Base(filepath.Clean(p))
		if !strings.HasPrefix(base, "backup-") || !strings.HasSuffix(base, ".tar.gz") {
			continue
		}
		host := filepath.Join(backupDir, base)
		info, err := os.Stat(host)
		if err != nil {
			continue
		}
		if info.ModTime().Unix() >= newestMtime {
			newestHost, newestMtime = host, info.ModTime().Unix()
		}
	}
	if newestHost != "" {
		return newestHost
	}
	return s.newestArchiveSince(ctx, at)
}

// moveBackupDir returns the effective archive root the on-node offen agent
// writes to (mounted as /archive inside the agent).
func (s *Service) moveBackupDir() string {
	if s.BackupDir != "" {
		return s.BackupDir
	}
	return backups.DefaultArchiveDir
}

// newestArchiveSince returns the newest backup-*.tar.gz in the archive root
// whose mtime is at or after the given unix time (control-plane archives are
// excluded). Empty string when none matches.
func (s *Service) newestArchiveSince(ctx context.Context, at int64) string {
	entries, err := os.ReadDir(s.moveBackupDir())
	if err != nil {
		return ""
	}
	var newest string
	var newestMtime int64
	for _, e := range entries {
		if e.IsDir() ||
			!strings.HasSuffix(e.Name(), ".tar.gz") ||
			!strings.HasPrefix(e.Name(), "backup-") ||
			strings.HasPrefix(e.Name(), "pmcluster-ctlplane-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		m := info.ModTime().Unix()
		if m >= at && m >= newestMtime {
			newest, newestMtime = filepath.Join(s.moveBackupDir(), e.Name()), m
		}
	}
	return newest
}

// extractStackSubtree extracts the <stack>/ subtree out of a whole-disk backup
// archive into volumeRoot, mapping entries through the baked-in `backup/data/`
// prefix (mirroring backups.archiveRelPath) and rejecting anything outside
// volumeRoot. Returns the number of regular files restored.
func extractStackSubtree(archivePath, volumeRoot, stackName string) (int, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var tr *tar.Reader
	if strings.HasSuffix(archivePath, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return 0, fmt.Errorf("gzip open: %w", err)
		}
		defer func() { _ = gz.Close() }()
		tr = tar.NewReader(gz)
	} else {
		tr = tar.NewReader(f)
	}

	include := func(rel string) bool {
		return rel == stackName || strings.HasPrefix(rel, stackName+"/")
	}

	var count int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		rel := moveArchiveRelPath(hdr.Name)
		if !include(rel) {
			continue
		}
		name := filepath.Join(volumeRoot, rel)
		if !strings.HasPrefix(name, volumeRoot+string(filepath.Separator)) {
			return count, fmt.Errorf("archive entry escapes volume root: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(name, 0o755); err != nil {
				return count, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return count, err
			}
			out, err := os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return count, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return count, err
			}
			out.Close()
			count++
		}
	}
	return count, nil
}

// moveArchiveRelPath strips the baked-in `backup/data/` prefix from an archive
// entry (the offen agent mounts VolumeRoot at /backup/data), mirroring
// backups.archiveRelPath. Entries without the prefix are used as-is.
func moveArchiveRelPath(name string) string {
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(name)), "/")
	const prefix = "backup/data"
	if clean == prefix {
		return "."
	}
	if strings.HasPrefix(clean, prefix+"/") {
		return strings.TrimPrefix(clean, prefix+"/")
	}
	return clean
}

// moveViaMover transits the stack's subtree to a remote target node: it serves
// the backup archive over an ephemeral HTTP server on this host (a random
// tokenized URL), then creates a one-shot swarm service pinned to the target
// that wget's the archive, extracts it and moves the <stack>/ subtree into the
// shared VolumeRoot bind. The mover uses the offen image (already present on
// every node; busybox wget/tar/mv).
func (s *Service) moveViaMover(ctx context.Context, archivePath, volumeRoot, stackName, targetNode, leaderAddr string) error {
	if leaderAddr == "" {
		return fmt.Errorf("no swarm leader address to reach the target node")
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	tokenHex := hex.EncodeToString(token)
	base := filepath.Base(archivePath)
	urlPath := "/move/" + tokenHex + "/" + base

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() { _ = ln.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc(urlPath, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, archivePath)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	port := ln.Addr().(*net.TCPAddr).Port
	// The mover runs on the target node and must reach THIS host over HTTP.
	// The swarm advertise address (leaderAddr) may be a public IP that
	// hardened nodes cannot route to each other on ephemeral ports, so build
	// candidate URLs from the advertise address PLUS every local interface
	// IP (private networks included) and let the mover try each in order.
	urls := moverCandidateURLs(leaderAddr, port, urlPath)
	if len(urls) == 0 {
		return fmt.Errorf("no candidate addresses to serve the move archive")
	}
	quoted := make([]string, 0, len(urls))
	for _, u := range urls {
		quoted = append(quoted, "'"+u+"'")
	}
	urlList := strings.Join(quoted, " ")

	svcName := fmt.Sprintf("pmcluster-move-%s-%s", stackName, tokenHex[:8])
	script := fmt.Sprintf(
		"mkdir -p /tmp/x /data && for u in %s; do wget -q -T 10 -O /tmp/m.tgz \"$u\" && break; done && tar -xzf /tmp/m.tgz -C /tmp/x && rm -rf /data/%s && mv /tmp/x/backup/data/%s /data/ && rm -rf /tmp/x",
		urlList, stackName, stackName,
	)

	create := exec.CommandContext(ctx, "docker", "service", "create",
		"--name", svcName,
		"--constraint", "node.hostname=="+targetNode,
		"--restart-condition", "none",
		"--mount", "type=bind,source="+volumeRoot+",destination=/data",
		"--entrypoint", "/bin/sh",
		"offen/docker-volume-backup:v2",
		"-c", script,
	)
	if out, err := create.CombinedOutput(); err != nil {
		return fmt.Errorf("docker service create: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = exec.CommandContext(rmCtx, "docker", "service", "rm", svcName).CombinedOutput()
	}()

	return waitForMoverTask(ctx, svcName, targetNode)
}

// moverCandidateURLs returns the http URLs a mover task on another node can
// use to reach THIS host's ephemeral archive server. The archive server binds
// 0.0.0.0, so any of these addresses serves it.
//
// Order matters: local interface IPs are tried BEFORE the swarm advertise
// address. The advertise address is often a public IP that hardened nodes
// cannot route to each other on ephemeral ports (it silently drops SYN), and
// busybox wget cannot reliably bound the connect on a dropped SYN — so a
// public-first order would hang the mover until the task deadline. Local
// RFC1918 (private) addresses are the most likely to be reachable between
// nodes, so they go first; the advertise address is the fallback for clusters
// that only reach each other through it.
func moverCandidateURLs(leaderAddr string, port int, urlPath string) []string {
	var urls []string
	seen := map[string]bool{}
	add := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		urls = append(urls, fmt.Sprintf("http://%s:%d%s", host, port, urlPath))
	}

	// Local interface IPs first, with private (RFC1918) addresses ahead of
	// public ones so the most-reachable candidate is tried first.
	ifaces, err := net.Interfaces()
	if err == nil {
		var privates, others []string
		for _, iface := range ifaces {
			addrs, aerr := iface.Addrs()
			if aerr != nil {
				continue
			}
			for _, a := range addrs {
				ipnet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				ip := ipnet.IP
				if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
					continue
				}
				host := ip.String()
				if seen[host] {
					continue
				}
				seen[host] = true
				if isRFC1918(ip) {
					privates = append(privates, host)
				} else {
					others = append(others, host)
				}
			}
		}
		for _, h := range privates {
			urls = append(urls, fmt.Sprintf("http://%s:%d%s", h, port, urlPath))
		}
		for _, h := range others {
			urls = append(urls, fmt.Sprintf("http://%s:%d%s", h, port, urlPath))
		}
	}

	// Advertise address last (fallback for clusters that only route via it).
	if host, _, serr := net.SplitHostPort(leaderAddr); serr == nil {
		add(host)
	} else {
		add(leaderAddr)
	}
	return urls
}

// isRFC1918 reports whether ip is a private IPv4 address (10/8, 172.16/12,
// 192.168/16) — the ranges most commonly used for inter-node private
// networks, and therefore the most likely to be reachable between peers.
func isRFC1918(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		}
	}
	return false
}
