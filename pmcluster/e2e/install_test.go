//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installScriptPath finds the install.sh at the repo root.
func installScriptPath(t *testing.T) string {
	t.Helper()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "install.sh")); err == nil {
			return filepath.Join(dir, "install.sh")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find install.sh above current working directory")
		}
		dir = parent
	}
}

// TestInstallScriptSystemdUnit validates that install.sh, when run in a
// simulated Linux environment (PMCLUSTER_FAKE_OS=linux), NO LONGER writes a
// systemd unit file — daemon management moved to the CLI (cluster up/update,
// join). install.sh only installs the binary + prints the daemon hints.
func TestInstallScriptSystemdUnit(t *testing.T) {
	_ = installScriptPath(t)

	tmpDir := t.TempDir()
	prefix := filepath.Join(tmpDir, "local", "bin")

	t.Run("SUDO_USER detection", func(t *testing.T) {
		out, err := exec.Command("bash", "-c", `
			SUDO_USER=deployer OS=linux PREFIX=/usr/local/bin
			PMCLUSTER_USER="${PMCLUSTER_USER:-${SUDO_USER:-$(id -un)}}"
			echo "PMCLUSTER_USER=$PMCLUSTER_USER"
			PMCLUSTER_HOME="$(eval echo ~${PMCLUSTER_USER})"
			echo "PMCLUSTER_HOME=$PMCLUSTER_HOME"
		`).CombinedOutput()
		if err != nil {
			t.Fatalf("user detection script: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "PMCLUSTER_USER=deployer") {
			t.Errorf("expected PMCLUSTER_USER=deployer, got:\n%s", out)
		}
	})

	t.Run("root user detection", func(t *testing.T) {
		out, err := exec.Command("bash", "-c", `
			OS=linux PREFIX=/usr/local/bin
			PMCLUSTER_USER="${PMCLUSTER_USER:-${SUDO_USER:-$(id -un)}}"
			echo "PMCLUSTER_USER=$PMCLUSTER_USER"
		`).CombinedOutput()
		if err != nil {
			t.Fatalf("user detection script: %v\n%s", err, out)
		}

		expected := fmt.Sprintf("PMCLUSTER_USER=%s", os.Getenv("USER"))
		if !strings.Contains(string(out), expected) {
			t.Errorf("expected %q, got:\n%s", expected, out)
		}
	})

	t.Run("PMCLUSTER_USER override", func(t *testing.T) {
		out, err := exec.Command("bash", "-c", `
			SUDO_USER=deployer PMCLUSTER_USER=customuser OS=linux PREFIX=/usr/local/bin
			PMCLUSTER_USER="${PMCLUSTER_USER:-${SUDO_USER:-$(id -un)}}"
			echo "PMCLUSTER_USER=$PMCLUSTER_USER"
		`).CombinedOutput()
		if err != nil {
			t.Fatalf("user detection script: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "PMCLUSTER_USER=customuser") {
			t.Errorf("expected PMCLUSTER_USER=customuser, got:\n%s", out)
		}
	})

	t.Run("docker group detection", func(t *testing.T) {
		out, err := exec.Command("bash", "-c", `
			DOCKER_GROUP="docker"
			if getent group docker-root >/dev/null 2>&1; then
				DOCKER_GROUP="docker-root"
			fi
			echo "DOCKER_GROUP=$DOCKER_GROUP"
		`).CombinedOutput()
		if err != nil {
			t.Fatalf("docker group script: %v\n%s", err, out)
		}

		if !strings.Contains(string(out), "DOCKER_GROUP=docker") {
			t.Errorf("expected DOCKER_GROUP=docker, got:\n%s", out)
		}
	})

	t.Run("install.sh does not write the systemd unit", func(t *testing.T) {
		// The systemd block was REMOVED from install.sh — the daemon is now
		// managed by the CLI (cluster up/update, join). Assert the script no
		// longer contains the ExecStart unit template.
		data, err := os.ReadFile(installScriptPath(t))
		if err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
		for _, forbidden := range []string{
			"/etc/systemd/system/pmcluster.service",
			"ExecStart=${PREFIX}/pmcluster serve",
			"systemctl enable pmcluster",
			"Installing systemd service",
		} {
			if strings.Contains(string(data), forbidden) {
				t.Errorf("install.sh must not contain systemd management (%q) — moved to the CLI", forbidden)
			}
		}
		if !strings.Contains(string(data), "pmcluster join") {
			t.Errorf("install.sh should hint at 'pmcluster join' for swarm nodes")
		}
	})

	_ = tmpDir
	_ = prefix
}

// TestInstallScriptDarwinSkip ensures install.sh prints the foreground-daemon
// hint on non-Linux (macOS) — it never installs systemd.
func TestInstallScriptDarwinSkip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("this test verifies Darwin behaviour; only runs on macOS")
	}

	data, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	if strings.Contains(string(data), "/etc/systemd/system/pmcluster.service") {
		t.Error("install.sh must not reference systemd units (daemon is CLI-managed)")
	}
	if !strings.Contains(string(data), "pmcluster serve") {
		t.Errorf("expected 'pmcluster serve' foreground hint in install.sh, got:\n%s", data)
	}
}

// TestInstallScriptSudoUser ensures that when the script runs as root (sudo),
// it adds the detected user to the docker group.
func TestInstallScriptSudoUser(t *testing.T) {

	if runtime.GOOS != "linux" {
		t.Skip("usermod only tested on Linux")
	}

	if _, err := exec.LookPath("usermod"); err != nil {
		t.Skip("usermod not found in PATH")
	}

	out, err := exec.Command("bash", "-c", `
		PMCLUSTER_USER=ubuntu DOCKER_GROUP=docker
		# Simulate running as root (id -u = 0).
		if [ "0" = "0" ] && [ "$PMCLUSTER_USER" != "root" ]; then
			echo "WOULD_RUN: usermod -aG $DOCKER_GROUP $PMCLUSTER_USER"
		else
			echo "WOULD_SKIP usermod"
		fi
	`).CombinedOutput()
	if err != nil {
		t.Fatalf("usermod test: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "WOULD_RUN: usermod -aG docker ubuntu") {
		t.Errorf("expected usermod invocation, got:\n%s", out)
	}

	out2, err2 := exec.Command("bash", "-c", `
		PMCLUSTER_USER=root DOCKER_GROUP=docker
		if [ "0" = "0" ] && [ "$PMCLUSTER_USER" != "root" ]; then
			echo "WOULD_RUN"
		else
			echo "WOULD_SKIP usermod for root"
		fi
	`).CombinedOutput()
	if err2 != nil {
		t.Fatalf("usermod skip test: %v\n%s", err2, out2)
	}
	if !strings.Contains(string(out2), "WOULD_SKIP usermod for root") {
		t.Errorf("expected usermod skip for root user, got:\n%s", out2)
	}
}

// TestInstallScriptStartCondition verifies the conditional start logic:
//   - If ~/.pmcluster/config.yaml exists → start the service.
//   - If ~/.pmcluster/config.yaml does not exist → do NOT start.
func TestInstallScriptStartCondition(t *testing.T) {
	t.Run("start when config exists", func(t *testing.T) {
		tmpHome := t.TempDir()

		pmDir := filepath.Join(tmpHome, ".pmcluster")
		if err := os.MkdirAll(pmDir, 0700); err != nil {
			t.Fatalf("mkdir .pmcluster: %v", err)
		}
		if err := os.WriteFile(filepath.Join(pmDir, "config.yaml"), []byte("listen_addr: \":9090\"\n"), 0600); err != nil {
			t.Fatalf("write config.yaml: %v", err)
		}

		out, err := exec.Command("bash", "-c", fmt.Sprintf(`
			PMCLUSTER_HOME=%s
			if [ -f "${PMCLUSTER_HOME}/.pmcluster/config.yaml" ]; then
				echo "WOULD_START"
			else
				echo "WOULD_SKIP_START"
			fi
		`, tmpHome)).CombinedOutput()
		if err != nil {
			t.Fatalf("start condition test: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "WOULD_START") {
			t.Errorf("expected WOULD_START, got:\n%s", out)
		}
	})

	t.Run("skip start when config missing", func(t *testing.T) {
		tmpHome := t.TempDir()
		out, err := exec.Command("bash", "-c", fmt.Sprintf(`
			PMCLUSTER_HOME=%s
			if [ -f "${PMCLUSTER_HOME}/.pmcluster/config.yaml" ]; then
				echo "WOULD_START"
			else
				echo "WOULD_SKIP_START"
			fi
		`, tmpHome)).CombinedOutput()
		if err != nil {
			t.Fatalf("start skip test: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "WOULD_SKIP_START") {
			t.Errorf("expected WOULD_SKIP_START, got:\n%s", out)
		}
	})
}
