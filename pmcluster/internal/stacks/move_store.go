package stacks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

// moverTaskTimeout bounds how long a one-shot mover task may take.
const moverTaskTimeout = 240 * time.Second

// storePullMoverImage is the mover image for the store transit: rclone (to pull
// the archive object from the store's S3 API) plus busybox tar/sh (verified to
// strip the archive's leading "/", so entries land at backup/data/<stack>/...).
const storePullMoverImage = "rclone/rclone:latest"

// waitForMoverTask polls a one-shot mover service until its task completes
// (nil), fails (error) or the deadline passes. Shared by both mover flavours.
func waitForMoverTask(ctx context.Context, svcName, targetNode string) error {
	deadline := time.Now().Add(moverTaskTimeout)
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

// removeMoverService force-removes a one-shot mover service (best effort).
func removeMoverService(svcName string) {
	rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = exec.CommandContext(rmCtx, "docker", "service", "rm", svcName).CombinedOutput()
}

// rcloneStoreEnv is the mover env: an rclone S3 remote pointing at the
// configured object store. For the in-cluster backup store that is the
// published loopback ingress (http://127.0.0.1:8333), which resolves on every
// node through the swarm routing mesh — no cross-node host ports involved.
func rcloneStoreEnv(cfg backups.S3Config) []string {
	endpoint := cfg.Endpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	return []string{
		"RCLONE_CONFIG_SW_TYPE=s3",
		"RCLONE_CONFIG_SW_PROVIDER=Other",
		"RCLONE_CONFIG_SW_ENDPOINT=" + endpoint,
		"RCLONE_CONFIG_SW_ACCESS_KEY_ID=" + cfg.AccessKey,
		"RCLONE_CONFIG_SW_SECRET_ACCESS_KEY=" + cfg.SecretKey,
		"RCLONE_CONFIG_SW_REGION=" + region,
		"RCLONE_CONFIG_SW_FORCE_PATH_STYLE=true",
	}
}

// storePullMoverScript is the mover program: pull the archive object with
// rclone, unpack it and move the <stack>/ subtree into the mounted volume root.
func storePullMoverScript(bucket, key, stackName string) string {
	return fmt.Sprintf(
		"set -e; mkdir -p /tmp/x /data; rclone copyto sw:%s/%s /tmp/m.tgz; tar -xzf /tmp/m.tgz -C /tmp/x; rm -rf /data/%s; mv /tmp/x/backup/data/%s /data/; rm -rf /tmp/x",
		bucket, key, stackName, stackName,
	)
}

// storePullMoverArgs builds the argv after `docker service create`.
func storePullMoverArgs(svcName, volumeRoot, targetNode string, env []string, script string) []string {
	args := []string{
		"--name", svcName,
		"--constraint", "node.hostname==" + targetNode,
		"--restart-condition", "none",
		"--mount", "type=bind,source=" + volumeRoot + ",destination=/data",
		"--host", "host.docker.internal:host-gateway", // service create flag is --host (update uses --host-add)
		"--entrypoint", "/bin/sh",
	}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	return append(args, storePullMoverImage, "-c", script)
}

// newestArchiveObject returns the key of the newest whole-disk archive in the
// configured object store WITHOUT downloading it — the point of the store
// transit (BUG-030).
func (s *Service) newestArchiveObject(ctx context.Context, stackName string) (string, error) {
	if !s.S3.Configured() {
		return "", fmt.Errorf("move: %s cannot move between nodes without a shared archive — configure the object store (backup_s3_* or the in-cluster backup store)", stackName)
	}
	objs, err := backups.ListS3Objects(ctx, s.S3, "backup-")
	if err != nil {
		return "", fmt.Errorf("move: list archives in the object store: %w", err)
	}
	var key string
	var newest time.Time
	for _, o := range objs {
		// Only whole-disk volume archives: never a control-plane archive
		// (pmcluster-ctlplane-*), whose contents live under backup/pmcluster
		// and would mis-restore. The server-side prefix filter does this too;
		// keep the client-side check as the authority.
		if !strings.HasPrefix(o.Key, "backup-") || !strings.HasSuffix(o.Key, ".tar.gz") {
			continue
		}
		if key == "" || o.LastModified.After(newest) {
			key, newest = o.Key, o.LastModified
		}
	}
	if key == "" {
		return "", fmt.Errorf("move: no whole-disk archive found in the object store (run `pmcluster backup create` first)")
	}
	return key, nil
}

// moveViaStorePull runs a one-shot mover service on the target node that pulls
// the archive object straight from the object store and unpacks the stack's
// subtree. No data crosses node-to-node host ports, so it needs no firewall
// rules — unlike the HTTP mover (BUG-030).
func (s *Service) moveViaStorePull(ctx context.Context, key, volumeRoot, stackName, targetNode string) error {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	svcName := fmt.Sprintf("pmcluster-move-%s-%s", stackName, hex.EncodeToString(token)[:8])
	script := storePullMoverScript(s.S3.Bucket, key, stackName)
	moverCfg := s.S3
	moverCfg.Endpoint = moverEndpoint(moverCfg.Endpoint) // container cannot use 127.0.0.1
	args := append([]string{"service", "create"},
		storePullMoverArgs(svcName, volumeRoot, targetNode, rcloneStoreEnv(moverCfg), script)...)

	if err := startMoverService(ctx, args, svcName); err != nil {
		return fmt.Errorf("docker service create: %w", err)
	}
	defer removeMoverService(svcName)

	return waitForMoverTask(ctx, svcName, targetNode)
}

// moverEndpoint rewrites a loopback object-store endpoint to the Docker
// host-gateway alias: the mover runs in a container, where 127.0.0.1 is the
// container itself — the store is published on the routing mesh, so
// host.docker.internal:<port> reaches it from any node (BUG-030 follow-up).
func moverEndpoint(endpoint string) string {
	e := endpoint
	for _, host := range []string{"127.0.0.1", "localhost"} {
		e = strings.Replace(e, "//"+host+":", "//host.docker.internal:", 1)
		e = strings.Replace(e, "//"+host+"/", "//host.docker.internal/", 1)
	}
	return e
}

// startMoverService runs `docker service create` without ever waiting on its
// inherited pipes. On some daemons the CLI does not return even after the
// service has been created (it stays attached to stdout/stderr), which stalled
// the whole move: the create call never returned, so the task-wait never began
// and the mover service was never cleaned up (BUG-030 third follow-up).
// We redirect the CLI output to a temp file, return as soon as the service
// exists (`docker service inspect`), and let the (possibly still attached) CLI
// be killed/reaped in the background.
func startMoverService(ctx context.Context, args []string, svcName string) error {
	logf, err := os.CreateTemp("", "pmcluster-mover-create-*.log")
	if err != nil {
		return err
	}
	defer func() {
		_ = logf.Close()
		_ = os.Remove(logf.Name())
	}()

	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel() // kills the CLI if it is still attached when we return

	cmd := exec.CommandContext(cctx, "docker", args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		return err
	}
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()

	exists := func() bool {
		return exec.CommandContext(cctx, "docker", "service", "inspect", svcName).Run() == nil
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if exists() {
			// Created. The create CLI may still be attached — harmless, and the
			// deferred cancel reaps it.
			return nil
		}
		select {
		case <-reaped:
			if exists() {
				return nil
			}
			data, _ := os.ReadFile(logf.Name())
			return fmt.Errorf("exit status %v: %s", exitStatus(cmd), strings.TrimSpace(string(data)))
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	data, _ := os.ReadFile(logf.Name())
	return fmt.Errorf("timed out creating the mover service: %s", strings.TrimSpace(string(data)))
}

// exitStatus reports the CLI's exit code when it has exited, "unknown" otherwise.
func exitStatus(cmd *exec.Cmd) any {
	if cmd.ProcessState == nil {
		return "unknown"
	}
	return cmd.ProcessState.ExitCode()
}
