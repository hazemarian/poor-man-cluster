package docker

import (
	"testing"

	"github.com/docker/docker/api/types/mount"
)

// TestShellLimits_TmpIsCappedAt200MiB pins the number an operator was told: a shell's /tmp is 200 MiB.
// Downloads land there, so this is the only writable space a session is allowed to fill.
func TestShellLimits_TmpIsCappedAt200MiB(t *testing.T) {
	if ShellTmpfsBytes != 200<<20 {
		t.Errorf("ShellTmpfsBytes = %d, want 200 MiB (%d)", ShellTmpfsBytes, 200<<20)
	}
	// A cap expressed in the wrong unit (200 << 10 is 200 KiB) would be a shell that cannot hold a
	// dump, and 200 << 30 a shell that can hold the node's disk. Both are one shift away.
	if ShellTmpfsBytes < 50<<20 || ShellTmpfsBytes > 2<<30 {
		t.Errorf("ShellTmpfsBytes = %d is implausible for a database dump", ShellTmpfsBytes)
	}
}

// TestShellLimits_MemoryBoundsTheTmpfs — a sized tmpfs without a memory limit moves the cost from the
// node's disk to the node's memory, which is not obviously better. The limit is what keeps an
// oversized write inside the throwaway container.
func TestShellLimits_MemoryBoundsTheTmpfs(t *testing.T) {
	if ShellMemoryBytes <= ShellTmpfsBytes {
		t.Errorf("memory limit %d must exceed the tmpfs %d: the clients need room the tmpfs does not "+
			"account for, or every session is OOM-killed as soon as it fills /tmp",
			ShellMemoryBytes, ShellTmpfsBytes)
	}
	// Headroom for the shell, curl and the postgres/mysql clients on top of a full /tmp.
	if ShellMemoryBytes-ShellTmpfsBytes < 128<<20 {
		t.Errorf("only %d bytes of headroom above the tmpfs; the clients need more",
			ShellMemoryBytes-ShellTmpfsBytes)
	}
}

// TestShellMounts_OneSizedTmpfsOnTmp — the mount itself, not just the constant: the cap only exists if
// it is attached to the task.
func TestShellMounts_OneSizedTmpfsOnTmp(t *testing.T) {
	mounts := shellMounts()
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want exactly one (/tmp)", len(mounts))
	}
	m := mounts[0]
	if m.Type != mount.TypeTmpfs {
		t.Errorf("mount type = %q, want tmpfs: a bind or volume mount would not be size-limited", m.Type)
	}
	if m.Target != "/tmp" {
		t.Errorf("mount target = %q, want /tmp: that is where downloads land", m.Target)
	}
	if m.TmpfsOptions == nil || m.TmpfsOptions.SizeBytes != ShellTmpfsBytes {
		t.Errorf("tmpfs options = %+v, want SizeBytes=%d", m.TmpfsOptions, ShellTmpfsBytes)
	}
	if m.TmpfsOptions.Mode != 0o1777 {
		t.Errorf("tmpfs mode = %o, want 1777 (sticky, writable inside the shell)", m.TmpfsOptions.Mode)
	}
}
