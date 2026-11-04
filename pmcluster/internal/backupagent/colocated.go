package backupagent

import (
	"bufio"
	"os"
	"sort"
	"strings"
)

// SplitStacks reports stacks whose data is NOT entirely inside the volume that
// gets snapshotted.
//
// A snapshot is only a consistent image of one filesystem. If a stack mounts a
// second volume — a WAL directory on its own disk, an NFS share, a tmpfs scratch
// area — then the snapshot captures half of that stack, and the half it misses is
// exactly the part a database needs to recover. The backup looks successful, is
// verified, ships, and cannot be restored: the silent failure this function exists
// to name.
//
// mounts is /proc/self/mounts; root is the volume root. The return value is sorted
// stack names, so the report is stable between runs.
func SplitStacks(mounts []byte, root string) []string {
	root = strings.TrimSuffix(root, "/")
	if root == "" {
		return nil
	}
	prefix := root + "/"
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(mounts)))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		// /proc/self/mounts escapes spaces and tabs in the mountpoint.
		point := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`).Replace(fields[1])
		if !strings.HasPrefix(point, prefix) {
			continue
		}
		rest := strings.TrimPrefix(point, prefix)
		name := strings.SplitN(rest, "/", 2)[0]
		if name != "" {
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// mountsFor reads the mount table, honouring MountsFile when set (tests).
func mountsFor(path string) []byte {
	if path != "" {
		b, _ := os.ReadFile(path)
		return b
	}
	return readMounts()
}

// readMounts is the production entry point: a missing or unreadable table must
// never stop a backup, so it degrades to "nothing known to be split".
func readMounts() []byte {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return nil
	}
	return b
}
