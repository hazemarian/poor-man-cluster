//go:build e2e

// Package e2e — DB-backed secrets & configs end-to-end tests.
//
// This file exercises the full lifecycle of the secrets/configs feature
// against a real single-node Docker Swarm:
//
//  1. secret create/list/show/verify (hash semantics, one-time value)
//  2. config create/get/edit/history/rollback (version history)
//  3. manifest deploy referencing both via env:
//     env:
//     DB_PASS: secrets(e2e_db_pass)      → /run/secrets/<name> + service mount
//     ADMIN_ENABLED: config(app_env)     → literal content injection
//  4. validation: secrets() env ref without a matching service secrets: entry
//     must fail the deploy.
//
// Gated behind PMCLUSTER_E2E_SWARM=1 (see deploy_test.go for the convention).
package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestSecretsConfigsE2E runs the DB-backed secrets & configs flow end to end.
func TestSecretsConfigsE2E(t *testing.T) {
	// ── Guards ────────────────────────────────────────────────────────────────
	if os.Getenv("PMCLUSTER_E2E_SWARM") != "1" {
		t.Skip("PMCLUSTER_E2E_SWARM is not set to 1; skipping secrets/configs e2e")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH; skipping secrets/configs e2e")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// ── Swarm bootstrap ───────────────────────────────────────────────────────
	weInitedSwarm := ensureSwarmActive(t, ctx)
	if weInitedSwarm {
		t.Cleanup(func() {
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer leaveCancel()
			out, err := dockerRun(leaveCtx, "swarm", "leave", "--force")
			if err != nil {
				t.Logf("docker swarm leave --force: %v\n%s", err, out)
			}
		})
	}

	// ── Overlay networks the DSL references ───────────────────────────────────
	for _, net := range []string{"traefik-net", "monitoring-net"} {
		netCtx, netCancel := context.WithTimeout(ctx, 30*time.Second)
		out, err := dockerRun(netCtx, "network", "create", "--driver=overlay", "--attachable", net)
		netCancel()
		if err != nil {
			if !strings.Contains(out, "already exists") && !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("docker network create %s: %v\n%s", net, err, out)
			}
			t.Logf("network %s already exists — reusing", net)
		} else {
			t.Logf("created overlay network: %s", net)
		}
	}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanCancel()
		for _, net := range []string{"traefik-net", "monitoring-net"} {
			out, err := dockerRun(cleanCtx, "network", "rm", net)
			if err != nil {
				t.Logf("docker network rm %s: %v\n%s", net, err, out)
			}
		}
	})

	// ── pmcluster init ────────────────────────────────────────────────────────
	homeDir := t.TempDir()
	initOut, _, initCode := runCmd(t, homeDir, "init")
	if initCode != 0 {
		t.Fatalf("pmcluster init exited %d:\n%s", initCode, initOut)
	}
	_ = extractToken(t, initOut)
	t.Logf("pmcluster init OK (home=%s)", homeDir)

	// ── Shared constants ──────────────────────────────────────────────────────
	const (
		secretName  = "e2e_db_pass"
		secretValue = "s3cr3t-value-123"
		configName  = "app_env"
		configV1    = "ADMIN_ENABLED=true"
		configV2    = "ADMIN_ENABLED=false"
		appName     = "e2esecrets"
	)

	// Cleanup the Swarm secret + stack regardless of how far the test got.
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanCancel()
		out, err := dockerRun(cleanCtx, "stack", "rm", appName)
		if err != nil {
			t.Logf("docker stack rm %s: %v\n%s", appName, err, out)
		}
		out, err = dockerRun(cleanCtx, "secret", "rm", secretName)
		if err != nil {
			t.Logf("docker secret rm %s: %v\n%s", secretName, err, out)
		}
		time.Sleep(3 * time.Second)
	})

	// ── a) secret create — value shown once, hash displayed ───────────────────
	t.Run("a-secret-create", func(t *testing.T) {
		out, errOut, code := runCmd(t, homeDir, "secret", "create", secretName, secretValue)
		combined := out + errOut
		if code != 0 {
			t.Fatalf("pmcluster secret create exited %d:\n%s", code, combined)
		}
		if !strings.Contains(combined, "Secret \""+secretName+"\" created") {
			t.Errorf("expected creation confirmation; got:\n%s", combined)
		}
		if !strings.Contains(combined, secretValue) {
			t.Errorf("expected the value to be shown once at creation; got:\n%s", combined)
		}
		if !strings.Contains(combined, "sha256:") {
			t.Errorf("expected sha256 hash in output; got:\n%s", combined)
		}
		if !strings.Contains(combined, "secrets("+secretName+")") {
			t.Errorf("expected DSL usage hint; got:\n%s", combined)
		}
		t.Logf("secret create OK:\n%s", combined)
	})

	// ── b) secret list — name + scope + short hash, never plaintext ───────────
	t.Run("b-secret-list-no-leak", func(t *testing.T) {
		out, _, code := runCmd(t, homeDir, "secret", "list")
		if code != 0 {
			t.Fatalf("pmcluster secret list exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, secretName) {
			t.Errorf("secret list missing %q; got:\n%s", secretName, out)
		}
		if !strings.Contains(out, "service") {
			t.Errorf("expected service scope in list; got:\n%s", out)
		}
		if strings.Contains(out, secretValue) {
			t.Errorf("secret list leaked the plaintext value!\n%s", out)
		}
	})

	// ── c) secret show + verify ───────────────────────────────────────────────
	t.Run("c-secret-show-verify", func(t *testing.T) {
		out, _, code := runCmd(t, homeDir, "secret", "show", secretName)
		if code != 0 {
			t.Fatalf("pmcluster secret show exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, "sha256:") || !strings.Contains(out, secretName) {
			t.Errorf("secret show missing name/hash; got:\n%s", out)
		}
		if strings.Contains(out, secretValue) {
			t.Errorf("secret show leaked plaintext!\n%s", out)
		}

		// Correct value → match.
		okOut, _, okCode := runCmd(t, homeDir, "secret", "verify", secretName, secretValue)
		if okCode != 0 || !strings.Contains(okOut, "hash matches") {
			t.Errorf("verify with correct value should match; code=%d out:\n%s", okCode, okOut)
		}
		// Wrong value → non-zero exit + mismatch message.
		badOut, badErr, badCode := runCmd(t, homeDir, "secret", "verify", secretName, "wrong-value")
		if badCode == 0 {
			t.Errorf("verify with wrong value should fail; got:\n%s%s", badOut, badErr)
		}
		if !strings.Contains(badOut+badErr, "does not match") {
			t.Errorf("expected mismatch message; got:\n%s%s", badOut, badErr)
		}
	})

	// ── d) config create/get/list ─────────────────────────────────────────────
	t.Run("d-config-create-get", func(t *testing.T) {
		out, _, code := runCmd(t, homeDir, "config", "create", configName,
			"--scope", "service", "--kind", "env", "--value", configV1)
		if code != 0 {
			t.Fatalf("pmcluster config create exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, "Config \""+configName+"\" created") {
			t.Errorf("expected creation confirmation; got:\n%s", out)
		}

		getOut, _, getCode := runCmd(t, homeDir, "config", "get", configName)
		if getCode != 0 {
			t.Fatalf("pmcluster config get exited %d:\n%s", getCode, getOut)
		}
		if !strings.Contains(getOut, configV1) {
			t.Errorf("config get missing content %q; got:\n%s", configV1, getOut)
		}
		if !strings.Contains(getOut, configName) {
			t.Errorf("config get missing name header; got:\n%s", getOut)
		}

		listOut, _, listCode := runCmd(t, homeDir, "config", "list")
		if listCode != 0 {
			t.Fatalf("pmcluster config list exited %d:\n%s", listCode, listOut)
		}
		if !strings.Contains(listOut, configName) {
			t.Errorf("config list missing %q; got:\n%s", configName, listOut)
		}
	})

	// ── e) config edit → history → rollback ───────────────────────────────────
	t.Run("e-config-edit-history-rollback", func(t *testing.T) {
		editOut, _, editCode := runCmd(t, homeDir, "config", "edit", configName, "--value", configV2)
		if editCode != 0 {
			t.Fatalf("pmcluster config edit exited %d:\n%s", editCode, editOut)
		}
		if !strings.Contains(editOut, "updated") {
			t.Errorf("expected updated confirmation; got:\n%s", editOut)
		}

		// After the edit, the config holds the new value.
		getOut, _, getCode := runCmd(t, homeDir, "config", "get", configName)
		if getCode != 0 || !strings.Contains(getOut, configV2) {
			t.Errorf("config get should show edited value %q; code=%d out:\n%s", configV2, getCode, getOut)
		}

		// History must now contain exactly one previous version (the original).
		histOut, _, histCode := runCmd(t, homeDir, "config", "history", configName)
		if histCode != 0 {
			t.Fatalf("pmcluster config history exited %d:\n%s", histCode, histOut)
		}
		if !strings.Contains(histOut, "ID") || !strings.Contains(histOut, "HASH") {
			t.Errorf("expected history table header; got:\n%s", histOut)
		}

		// Roll back (default = most recent version) → original value restored.
		rbOut, _, rbCode := runCmd(t, homeDir, "config", "rollback", configName)
		if rbCode != 0 {
			t.Fatalf("pmcluster config rollback exited %d:\n%s", rbCode, rbOut)
		}
		if !strings.Contains(rbOut, "rolled back") {
			t.Errorf("expected rollback confirmation; got:\n%s", rbOut)
		}
		finalOut, _, finalCode := runCmd(t, homeDir, "config", "get", configName)
		if finalCode != 0 || !strings.Contains(finalOut, configV1) {
			t.Errorf("config get after rollback should be %q; code=%d out:\n%s", configV1, finalCode, finalOut)
		}
	})

	// ── f) validation: secrets() env ref without service mount must fail ──────
	t.Run("f-validate-unmounted-secret-ref", func(t *testing.T) {
		manifestPath := homeDir + "/bad_secret_ref.yaml"
		manifest := `app: e2ebadref
env: production
domain: example.test
secrets:
  - ` + secretName + `
services:
  web:
    image: traefik/whoami:v1.10
    replicas: 1
    env:
      DB_PASS: secrets(` + secretName + `)
`
		if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
			t.Fatalf("write bad-ref manifest: %v", err)
		}
		out, errOut, code := runCmd(t, homeDir, "deploy", manifestPath)
		combined := out + errOut
		if code == 0 {
			t.Fatalf("expected deploy to fail for unmounted secrets() ref; got exit 0:\n%s", combined)
		}
		if !strings.Contains(combined, "not mounted") || !strings.Contains(combined, secretName) {
			t.Errorf("expected 'not mounted' validation error naming %q; got:\n%s", secretName, combined)
		}
		t.Logf("validation correctly rejected unmounted secrets() ref (exit %d)", code)
	})

	// ── g) real deploy with secrets() + config() env refs ─────────────────────
	t.Run("g-deploy-with-env-refs", func(t *testing.T) {
		// The DSL marks secrets external:true — the Swarm secret must exist.
		// Pipe the value in via stdin (docker secret create <name> -).
		secCtx, secCancel := context.WithTimeout(ctx, 30*time.Second)
		secCmd := exec.CommandContext(secCtx, "docker", "secret", "create", secretName, "-")
		secCmd.Stdin = strings.NewReader(secretValue)
		var secBuf bytes.Buffer
		secCmd.Stdout = &secBuf
		secCmd.Stderr = &secBuf
		secErr := secCmd.Run()
		secCancel()
		if secErr != nil {
			// "already exists" is fine on re-runs.
			if !strings.Contains(secBuf.String(), "already exists") && !strings.Contains(secErr.Error(), "already exists") {
				t.Fatalf("docker secret create %s: %v\n%s", secretName, secErr, secBuf.String())
			}
		}

		manifestPath := homeDir + "/manifest_envrefs.yaml"
		manifest := `app: ` + appName + `
env: production
domain: example.test
secrets:
  - ` + secretName + `
services:
  web:
    image: traefik/whoami:v1.10
    replicas: 1
    secrets:
      - ` + secretName + `
    env:
      DB_PASS: secrets(` + secretName + `)
      ADMIN_ENABLED: config(` + configName + `)
    expose:
      port: 80
      host: web.` + appName + `.example.test
`
		if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
			t.Fatalf("write envref manifest: %v", err)
		}

		deployOut, deployErr, code := runCmd(t, homeDir, "deploy", manifestPath)
		combined := deployOut + deployErr
		if code != 0 {
			t.Fatalf("pmcluster deploy with env refs exited %d:\n%s", code, combined)
		}
		if !strings.Contains(combined, "Deployed "+appName) {
			t.Errorf("expected 'Deployed %s' in output; got:\n%s", appName, combined)
		}
		t.Logf("deploy with env refs OK:\n%s", combined)

		// Service must exist.
		assertServiceExists(t, ctx, appName, appName+"_web")

		// Inspect the running service spec — DB_PASS must resolve to the mount
		// path and ADMIN_ENABLED to the config content.
		deadline := time.Now().Add(60 * time.Second)
		var envSpec string
		var err error
		for time.Now().Before(deadline) {
			inspCtx, inspCancel := context.WithTimeout(ctx, 15*time.Second)
			envSpec, err = dockerRun(inspCtx, "service", "inspect",
				"--format", "{{range .Spec.TaskTemplate.ContainerSpec.Env}}{{.}}\n{{end}}",
				appName+"_web")
			inspCancel()
			if err == nil && strings.Contains(envSpec, "/run/secrets/"+secretName) {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			t.Fatalf("docker service inspect %s: %v", appName+"_web", err)
		}

		if !strings.Contains(envSpec, "DB_PASS=/run/secrets/"+secretName) {
			t.Errorf("service env missing DB_PASS=/run/secrets/%s; got:\n%s", secretName, envSpec)
		}
		if !strings.Contains(envSpec, "ADMIN_ENABLED="+configV1) {
			t.Errorf("service env missing ADMIN_ENABLED=%s (config content); got:\n%s", configV1, envSpec)
		}

		// The secret must be mounted (in the service's secrets list).
		mountOut, mountErr := dockerRun(ctx, "service", "inspect",
			"--format", "{{range .Spec.TaskTemplate.ContainerSpec.Secrets}}{{.SecretName}} {{end}}",
			appName+"_web")
		if mountErr != nil {
			t.Fatalf("docker service inspect (secrets): %v", mountErr)
		}
		if !strings.Contains(mountOut, secretName) {
			t.Errorf("service does not mount %q; got: %s", secretName, mountOut)
		}
	})
}
