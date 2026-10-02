package stacks

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	if s.Docker != nil {
		nodes, err := s.Docker.NodeList(ctx)
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

	// 1. Whole-disk backup (unanchored row, so the restore's volume filter can
	// match the <stack>/ prefix via the bare-volume branch of volumeMatch).
	archivePath, err := s.runMoveBackup(ctx)
	if err != nil {
		return err
	}

	// 2. Restore the stack's subtree on the target.
	volRoot := s.VolumeRoot
	if volRoot == "" {
		volRoot = manifest.DefaultVolumeRoot
	}
	if s.Docker == nil || targetNode == localHost {
		if _, err := extractStackSubtree(archivePath, volRoot, stackName); err != nil {
			return fmt.Errorf("move: restore %s subtree: %w", stackName, err)
		}
	} else {
		if err := s.moveViaMover(ctx, archivePath, volRoot, stackName, targetNode, leaderAddr); err != nil {
			return fmt.Errorf("move: mover transit: %w", err)
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
	return nil
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
	url := fmt.Sprintf("http://%s:%d%s", leaderAddr, port, urlPath)

	svcName := fmt.Sprintf("pmcluster-move-%s-%s", stackName, tokenHex[:8])
	script := fmt.Sprintf(
		"mkdir -p /tmp/x /data && wget -qO /tmp/m.tgz '%s' && tar -xzf /tmp/m.tgz -C /tmp/x && mv /tmp/x/backup/data/%s /data/ && rm -rf /tmp/x",
		url, stackName,
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

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		psCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		out, _ := exec.CommandContext(psCtx, "docker", "service", "ps", svcName, "--format", "{{.CurrentState}}").CombinedOutput()
		cancel()
		state := string(out)
		if strings.Contains(state, "Failed") {
			return fmt.Errorf("mover task failed on %s: %s", targetNode, strings.TrimSpace(state))
		}
		if strings.Contains(state, "Complete") || strings.Contains(state, "Shutdown") {
			return nil
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("timed out waiting for mover task on %s", targetNode)
}
